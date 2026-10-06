package storetest

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// A task that a crash left on a session after it expired is ended by the retention loop, so that the sweep that deletes
// expired sessions is not held back by it for ever (data protection, DP-4). The caller owns the horizon and what is done at
// the agent; the store owns what is stale, and what ending a task takes with it.

func (su suite) staleTasks(t *testing.T) {
	cutoff := t0.Add(24 * time.Hour)
	horizons := func(b string) store.SessionHorizons {
		return store.SessionHorizons{ByBinding: map[string]time.Time{b: cutoff}, Otherwise: cutoff}
	}
	// seed makes a session of binding last active at, recording task (none if empty) with a turn.
	seed := func(t *testing.T, s store.Store, thread, binding, task string, at time.Time) store.ConversationKey {
		t.Helper()
		k := keyOf(thread)
		noErr(t, s.UpsertSession(t.Context(), store.Session{Key: k, ContextID: "ctx-" + thread, Binding: binding, LastActivity: at}), "session "+thread)
		if task != "" {
			noErr(t, s.SetSessionTask(t.Context(), k, "", task, store.TaskTurn{ID: "turn-" + thread, User: "U1"}), "task of "+thread)
		}
		return k
	}
	// pend makes an interaction of task, pending, in the session k.
	pend := func(t *testing.T, s store.Store, id string, k store.ConversationKey, task string) {
		t.Helper()
		i := newInteraction(id, time.Hour)
		i.Key, i.TaskID = k, task
		_, err := s.CreateInteraction(t.Context(), i)
		noErr(t, err, "interaction "+id)
	}
	list := func(t *testing.T, s store.Store, h store.SessionHorizons, limit int) []store.StaleTask {
		t.Helper()
		got, err := s.ListStaleTasks(t.Context(), h, limit)
		noErr(t, err, "ListStaleTasks")
		return got
	}
	end := func(t *testing.T, s store.Store, h store.SessionHorizons, st store.StaleTask) store.EndedStaleTask {
		t.Helper()
		got, err := s.EndStaleTask(t.Context(), h, st, t0.Add(1000*time.Hour))
		noErr(t, err, "EndStaleTask")
		return got
	}
	task := func(t *testing.T, s store.Store, k store.ConversationKey) store.Session {
		t.Helper()
		got, err := s.GetSession(t.Context(), k, 1000*ttl, t0)
		noErr(t, err, "GetSession "+k.Thread)
		return got
	}
	state := func(t *testing.T, s store.Store, id string) store.InteractionState {
		t.Helper()
		return getInteraction(t, s, id).State
	}

	su.run(t, "ListStaleTasks lists the sessions past their horizon that record a task, the oldest first, and no other", func(t *testing.T, s store.Store) {
		seed(t, s, "1.1", "ns/b", "task-b", cutoff.Add(-time.Hour))
		seed(t, s, "2.2", "ns/b", "task-a", cutoff.Add(-2*time.Hour))
		seed(t, s, "3.3", "ns/b", "task-edge", cutoff)                  // the horizon is a time the session must be before
		seed(t, s, "4.4", "ns/b", "task-inside", cutoff.Add(time.Hour)) // inside it
		seed(t, s, "5.5", "ns/b", "", cutoff.Add(-3*time.Hour))         // past it, with no task: nothing to end
		want := []store.StaleTask{
			{Key: keyOf("2.2"), Binding: "ns/b", ContextID: "ctx-2.2", TaskID: "task-a"},
			{Key: keyOf("1.1"), Binding: "ns/b", ContextID: "ctx-1.1", TaskID: "task-b"},
		}
		got := list(t, s, horizons("ns/b"), 10)
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("ListStaleTasks = %+v; want %+v: the two past the horizon with a task, the older first", got, want)
		}
		if got := list(t, s, horizons("ns/b"), 1); len(got) != 1 || got[0] != want[0] {
			t.Errorf("ListStaleTasks with a limit of 1 = %+v; want the oldest, %+v", got, want[0])
		}
		for _, k := range []store.ConversationKey{keyOf("3.3"), keyOf("4.4")} { // listing changes nothing
			if task(t, s, k).TaskID == "" {
				t.Errorf("the task of %s after listing is gone; want listing to change nothing", k.Thread)
			}
		}
	})

	su.run(t, "ListStaleTasks gives each Binding its own horizon, and a Binding it is not told of the other one", func(t *testing.T, s store.Store) {
		seed(t, s, "1.1", "ns/long", "t1", t0.Add(time.Hour))
		seed(t, s, "2.2", "ns/short", "t2", t0.Add(time.Hour))
		seed(t, s, "3.3", "ns/left", "t3", t0.Add(time.Hour))
		seed(t, s, "4.4", "ns/left", "t4", t0.Add(3*time.Hour))
		h := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/long": t0, "ns/short": t0.Add(2 * time.Hour)}, Otherwise: t0.Add(2 * time.Hour)}
		got := list(t, s, h, 10)
		if len(got) != 2 || got[0].TaskID != "t2" || got[1].TaskID != "t3" {
			t.Errorf("ListStaleTasks = %+v; want the short Binding's and the one that left, and not the long one's", got)
		}
	})

	su.run(t, "ListStaleTasks with no Binding named applies the one horizon to every session", func(t *testing.T, s store.Store) {
		seed(t, s, "1.1", "ns/a", "t1", t0)
		seed(t, s, "2.2", "ns/b", "t2", t0.Add(2*time.Hour))
		if got := list(t, s, store.SessionHorizons{Otherwise: t0.Add(time.Hour)}, 10); len(got) != 1 || got[0].TaskID != "t1" {
			t.Errorf("ListStaleTasks = %+v; want the one before the horizon", got)
		}
	})

	su.run(t, "EndStaleTask clears the task and its turn and cancels the task's pending interactions, and only those", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		k := seed(t, s, "1.1", "ns/b", "task-1", cutoff.Add(-time.Hour))
		other := seed(t, s, "2.2", "ns/b", "task-1", cutoff.Add(-time.Hour)) // another session that names the same task
		// The same thread number on another connection, and in another channel: other sessions, whatever the thread says.
		twins := map[string]store.ConversationKey{
			"twin-conn": {Connection: "other", Channel: k.Channel, Thread: k.Thread}, "twin-channel": {Connection: k.Connection, Channel: "C2", Thread: k.Thread},
		}
		for id, tk := range twins {
			noErr(t, s.UpsertSession(ctx, store.Session{Key: tk, ContextID: "ctx-" + id, Binding: "ns/b", LastActivity: cutoff.Add(-time.Hour)}), id+" session")
			noErr(t, s.SetSessionTask(ctx, tk, "", "task-1", store.TaskTurn{}), id+" task")
			pend(t, s, id+"-ask", tk, "task-1")
		}
		pend(t, s, "mine", k, "task-1")
		pend(t, s, "mine-too", k, "task-1")
		pend(t, s, "older-task", k, "task-0") // another task of the same session
		pend(t, s, "elsewhere", other, "task-1")
		settled := newInteraction("settled", time.Hour)
		settled.Key, settled.TaskID = k, "task-1"
		_, err := s.CreateInteraction(ctx, settled)
		noErr(t, err, "settled")
		noErr(t, s.ResolveInteraction(ctx, "settled", store.InteractionTimedOut, settled.Deadline), "time out settled")
		before := task(t, s, k)

		at := t0.Add(1000 * time.Hour)
		got, err := s.EndStaleTask(ctx, horizons("ns/b"), store.StaleTask{Key: k, Binding: "ns/b", ContextID: "ctx-1.1", TaskID: "task-1"}, at)
		noErr(t, err, "EndStaleTask")
		if got != (store.EndedStaleTask{Ended: true, Interactions: 2}) {
			t.Fatalf("EndStaleTask = %+v; want the task ended, with its 2 pending interactions", got)
		}
		after := task(t, s, k)
		if after.TaskID != "" || after.Turn != (store.TaskTurn{}) || after.TurnTask != "" {
			t.Errorf("session after = task %q, turn %+v, turn task %q; want no task, and no turn", after.TaskID, after.Turn, after.TurnTask)
		}
		if after.ContextID != before.ContextID || !after.LastActivity.Equal(before.LastActivity) || after.Binding != before.Binding {
			t.Errorf("session after = %+v; want its context, Binding and last activity as they were, %+v", after, before)
		}
		for id, want := range map[string]store.InteractionState{
			"mine": store.InteractionCancelled, "mine-too": store.InteractionCancelled,
			"older-task": store.InteractionPending, "elsewhere": store.InteractionPending, "twin-conn-ask": store.InteractionPending, "twin-channel-ask": store.InteractionPending, "settled": store.InteractionTimedOut,
		} {
			if got := state(t, s, id); got != want {
				t.Errorf("interaction %s is %s; want %s", id, got, want)
			}
		}
		if d := getInteraction(t, s, "mine").DecidedAt; !d.Equal(at) {
			t.Errorf("the cancelled interaction was decided at %v; want the time given, %v", d, at)
		}
		for name, k := range map[string]store.ConversationKey{"another thread": other, "twin-conn": twins["twin-conn"], "twin-channel": twins["twin-channel"]} {
			if task(t, s, k).TaskID != "task-1" {
				t.Errorf("the task of the session %s was cleared; want it left", name)
			}
		}
		if again := end(t, s, horizons("ns/b"), store.StaleTask{Key: k, Binding: "ns/b", TaskID: "task-1"}); again != (store.EndedStaleTask{}) {
			t.Errorf("a second EndStaleTask = %+v; want nothing to do", again)
		}
	})

	su.run(t, "A session whose task has been ended is deleted by the next purge, with the interaction that was pending", func(t *testing.T, s store.Store) {
		h := house{keyOf("1.1"), "ns/b", cutoff.Add(-time.Hour)}
		h.build(t, s)
		noErr(t, s.SetSessionTask(t.Context(), h.key, "", "task-1", store.TaskTurn{}), "task")
		pend(t, s, "asking", h.key, "task-1")
		purge := func() store.PurgedSessions {
			t.Helper()
			got, err := s.PurgeExpiredSessions(t.Context(), horizons("ns/b"), 10)
			noErr(t, err, "PurgeExpiredSessions")
			return got
		}
		if got := purge(); got != (store.PurgedSessions{}) {
			t.Fatalf("purged %+v with a task recorded; want nothing: it holds the session", got)
		}
		stale := list(t, s, horizons("ns/b"), 10)
		if len(stale) != 1 {
			t.Fatalf("ListStaleTasks = %+v; want the one session", stale)
		}
		if got := end(t, s, horizons("ns/b"), stale[0]); !got.Ended || got.Interactions != 1 {
			t.Fatalf("EndStaleTask = %+v; want it ended, with the pending interaction", got)
		}
		if got := list(t, s, horizons("ns/b"), 10); len(got) != 0 {
			t.Errorf("ListStaleTasks after = %+v; want none", got)
		}
		want := store.PurgedSessions{Sessions: 1, Replies: 2, Interactions: 2, Presses: 1, Outbox: 4} // the house, and the interaction that was pending
		if got := purge(); got != want {
			t.Errorf("purged %+v after the task ended; want %+v", got, want)
		}
		h.whole(t, s, false)
	})

	su.run(t, "EndStaleTask changes nothing unless the session still records the task and is still past its horizon", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		pending := func(k store.ConversationKey, task string) { pend(t, s, "ask-"+k.Thread, k, task) }
		active := seed(t, s, "1.1", "ns/b", "task-1", cutoff.Add(-time.Hour))
		moved := seed(t, s, "2.2", "ns/b", "task-1", cutoff.Add(-time.Hour))
		edge := seed(t, s, "3.3", "ns/b", "task-1", cutoff)
		long := seed(t, s, "4.4", "ns/long", "task-1", t0.Add(time.Hour)) // past the horizon of ns/b, not of its own
		none := seed(t, s, "5.5", "ns/b", "", cutoff.Add(-time.Hour))
		for _, k := range []store.ConversationKey{active, moved, edge, long, none} {
			pending(k, "task-1")
		}
		noErr(t, s.TouchSession(ctx, active, cutoff.Add(time.Hour)), "the session is active again")
		noErr(t, s.SetSessionTask(ctx, moved, "", "task-2", store.TaskTurn{}), "another task")
		h := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": cutoff, "ns/long": t0}, Otherwise: cutoff}

		for name, k := range map[string]store.ConversationKey{
			"a session active since": active, "a session on another task": moved, "a session exactly at the horizon": edge,
			"a session of a Binding with a later horizon": long, "a session with no task": none,
		} {
			// The Binding the caller names is not the one that decides: the store reads the session's own.
			if got := end(t, s, h, store.StaleTask{Key: k, Binding: "ns/b", TaskID: "task-1"}); got != (store.EndedStaleTask{}) {
				t.Errorf("EndStaleTask of %s = %+v; want nothing done", name, got)
			}
			if got := state(t, s, "ask-"+k.Thread); got != store.InteractionPending {
				t.Errorf("the interaction of %s is %s after the refusal; want it pending", name, got)
			}
		}
		for name, c := range map[string]struct {
			k    store.ConversationKey
			want string
		}{"active since": {active, "task-1"}, "on another task": {moved, "task-2"}, "at the horizon": {edge, "task-1"}, "a later horizon": {long, "task-1"}, "no task": {none, ""}} {
			if got := task(t, s, c.k).TaskID; got != c.want {
				t.Errorf("the task of the session %s is %q; want %q, as it was", name, got, c.want)
			}
		}
		if got := end(t, s, h, store.StaleTask{Key: keyOf("9.9"), Binding: "ns/b", TaskID: "task-1"}); got != (store.EndedStaleTask{}) {
			t.Errorf("EndStaleTask of a session that is not there = %+v; want nothing done, and no error", got)
		}
	})

	su.run(t, "EndStaleTask refuses arguments that are not legal", func(t *testing.T, s store.Store) {
		k := seed(t, s, "1.1", "ns/b", "task-1", cutoff.Add(-time.Hour))
		ok := store.StaleTask{Key: k, Binding: "ns/b", TaskID: "task-1"}
		now := t0.Add(1000 * time.Hour)
		for name, c := range map[string]struct {
			h   store.SessionHorizons
			st  store.StaleTask
			now time.Time
		}{
			"no task":            {horizons("ns/b"), store.StaleTask{Key: k}, now},
			"an incomplete key":  {horizons("ns/b"), store.StaleTask{Key: store.ConversationKey{Channel: "C1", Thread: "1.1"}, TaskID: "task-1"}, now},
			"no time":            {horizons("ns/b"), ok, time.Time{}},
			"no Otherwise":       {store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": cutoff}}, ok, now},
			"a zero horizon":     {store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": {}}, Otherwise: cutoff}, ok, now},
			"no horizons at all": {store.SessionHorizons{}, ok, now},
		} {
			_, err := s.EndStaleTask(t.Context(), c.h, c.st, c.now)
			isErr(t, err, store.ErrInvalid, name)
		}
		for name, c := range map[string]struct {
			h     store.SessionHorizons
			limit int
		}{"no limit": {horizons("ns/b"), 0}, "a negative limit": {horizons("ns/b"), -1}, "no horizons": {store.SessionHorizons{}, 1}} {
			_, err := s.ListStaleTasks(t.Context(), c.h, c.limit)
			isErr(t, err, store.ErrInvalid, "ListStaleTasks with "+name)
		}
		if task(t, s, k).TaskID != "task-1" {
			t.Error("the task is gone after refused calls; want it left")
		}
	})
}
