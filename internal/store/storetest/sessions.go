package storetest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// sameSession compares what a session is, leaving out its trace context and comparing times as instants.
func sameSession(a, b store.Session) bool {
	return a.Key == b.Key && a.ContextID == b.ContextID && a.TaskID == b.TaskID && a.Turn == b.Turn && a.TurnTask == b.TurnTask && a.Binding == b.Binding &&
		a.LastActivity.Equal(b.LastActivity)
}

func keyID(k store.ConversationKey) string {
	return k.Connection + "\x00" + k.Channel + "\x00" + k.Thread
}

func keyIDs(keys []store.ConversationKey) []string {
	var ids []string
	for _, k := range keys {
		ids = append(ids, keyID(k))
	}
	slices.Sort(ids)
	return ids
}

func getSession(t *testing.T, s store.Store, k store.ConversationKey) store.Session {
	t.Helper()
	got, err := s.GetSession(t.Context(), k, 1000*ttl, t0)
	if err != nil {
		t.Fatalf("GetSession(%+v): %v", k, err)
	}
	return got
}

func (su suite) sessions(t *testing.T) {
	su.run(t, "a session is stored by its key and read back; ContextID and TaskID are optional", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		_, err := s.GetSession(ctx, key, ttl, t0)
		isErr(t, err, store.ErrNotFound, "GetSession of a missing session")
		sess := store.Session{Key: key, ContextID: "ctx1", TaskID: "task1", Binding: "ns/b", LastActivity: t0}
		noBinding, noKey := sess, sess
		noBinding.Binding = ""
		noKey.Key.Thread = ""
		for _, b := range []store.Session{{Key: key, Binding: "ns/b"}, noBinding, noKey} {
			isErr(t, s.UpsertSession(ctx, b), store.ErrInvalid, fmt.Sprintf("UpsertSession(%+v)", b))
		}
		_, err = s.GetSession(ctx, key, 0, t0)
		isErr(t, err, store.ErrInvalid, "GetSession with a zero ttl")
		_, err = s.GetSession(ctx, key, ttl, time.Time{})
		isErr(t, err, store.ErrInvalid, "GetSession with a zero now")

		noCtx := sess
		noCtx.ContextID, noCtx.TaskID = "", ""
		if err := s.UpsertSession(ctx, noCtx); err != nil {
			t.Fatalf("UpsertSession without ContextID or TaskID: %v", err)
		}
		if got, err := s.GetSession(ctx, key, ttl, t0); err != nil || !sameSession(got, noCtx) {
			t.Fatalf("GetSession = %+v, %v; want %+v", got, err, noCtx)
		}
	})

	su.run(t, "an upsert replaces the whole row", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		first := store.Session{Key: key, ContextID: "ctx1", TaskID: "task1", Binding: "ns/b", LastActivity: t0}
		second := store.Session{Key: key, ContextID: "", TaskID: "task2", Binding: "ns/other", LastActivity: t0.Add(time.Hour)}
		for _, sess := range []store.Session{first, second} {
			if err := s.UpsertSession(ctx, sess); err != nil {
				t.Fatalf("UpsertSession: %v", err)
			}
			if got := getSession(t, s, key); !sameSession(got, sess) {
				t.Fatalf("session = %+v; want %+v: an upsert replaces every column, an empty ContextID included", got, sess)
			}
		}
	})

	su.run(t, "sessions are told apart by connection, channel and thread, exactly", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		keys := []store.ConversationKey{
			{Connection: "slack", Channel: "C1", Thread: "1.1"},
			{Connection: "slack2", Channel: "C1", Thread: "1.1"},
			{Connection: "slack", Channel: "C2", Thread: "1.1"},
			{Connection: "slack", Channel: "C1", Thread: "1.2"},
			{Connection: "Slack", Channel: "C1", Thread: "1.1"},
			{Connection: "slack", Channel: "c1", Thread: "1.1"},
			{Connection: "slack", Channel: "C1", Thread: "1.1 "},
		}
		for i, k := range keys {
			if err := s.UpsertSession(ctx, store.Session{Key: k, Binding: fmt.Sprint("ns/b", i), LastActivity: t0}); err != nil {
				t.Fatal(err)
			}
		}
		for i, k := range keys {
			if got := getSession(t, s, k); got.Binding != fmt.Sprint("ns/b", i) {
				t.Errorf("session %+v has Binding %q; want ns/b%d: keys that differ in one part, or in case or spaces, are different sessions", k, got.Binding, i)
			}
		}
	})

	su.run(t, "a session exactly the ttl old is live, an older one is returned with ErrExpired and kept", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		sess := store.Session{Key: key, ContextID: "ctx1", TaskID: "task1", Binding: "ns/b", LastActivity: t0}
		noErr(t, s.UpsertSession(ctx, sess), "UpsertSession")
		if _, err := s.GetSession(ctx, key, ttl, t0.Add(ttl)); err != nil {
			t.Fatalf("GetSession at the boundary = %v; want live", err)
		}
		for range 2 { // reading never deletes
			got, err := s.GetSession(ctx, key, ttl, t0.Add(ttl+time.Microsecond))
			isErr(t, err, store.ErrExpired, "GetSession a microsecond past the ttl")
			if !sameSession(got, sess) {
				t.Fatalf("expired session = %+v; want the row with the error", got)
			}
		}
		// Touching extends the window.
		noErr(t, s.TouchSession(ctx, key, t0.Add(ttl)), "TouchSession")
		if _, err := s.GetSession(ctx, key, ttl, t0.Add(2*ttl)); err != nil {
			t.Fatalf("GetSession after touch = %v; want live", err)
		}
	})

	// TouchSession "sets" the activity: it is not a maximum.
	su.run(t, "TouchSession sets the activity to the time given, earlier or later", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, TaskID: "t", Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		for _, at := range []time.Time{t0.Add(time.Hour), t0.Add(-time.Hour)} {
			noErr(t, s.TouchSession(ctx, key, at), "TouchSession")
			if got := getSession(t, s, key); !got.LastActivity.Equal(at) || got.TaskID != "t" || got.Binding != "ns/b" {
				t.Errorf("session = %+v; want activity %v and nothing else changed", got, at)
			}
		}
		isErr(t, s.TouchSession(ctx, keyOf("9.9"), t0), store.ErrNotFound, "TouchSession of a missing session")
		isErr(t, s.TouchSession(ctx, store.ConversationKey{}, t0), store.ErrInvalid, "TouchSession with an empty key")
		isErr(t, s.TouchSession(ctx, key, time.Time{}), store.ErrInvalid, "TouchSession with no time")
	})

	su.run(t, "many upserts of one key leave one session", func(t *testing.T, s store.Store) {
		const n = 16
		parallel(n, func(i int) {
			sess := store.Session{Key: key, TaskID: fmt.Sprint("t", i), Binding: "ns/b", LastActivity: t0.Add(time.Duration(i) * time.Second)}
			if err := s.UpsertSession(t.Context(), sess); err != nil {
				t.Errorf("upsert %d: %v", i, err)
			}
		})
		active, err := s.ListActiveSessions(t.Context())
		if err != nil || len(active) != 1 {
			t.Fatalf("active sessions = %+v, %v; want the one", active, err)
		}
	})

	su.run(t, "writes of every kind from many goroutines all land", func(t *testing.T, s store.Store) {
		const workers, perWorker = 8, 25
		parallel(workers, func(w int) {
			for i := range perWorker {
				id := fmt.Sprintf("w%d-%d", w, i)
				if _, err := s.AppendEvent(t.Context(), newEvent(id, t0)); err != nil {
					t.Errorf("AppendEvent: %v", err)
				}
				o := store.OutboxEntry{ID: id, EventID: id, Role: store.OutboxRoot, Channel: "C1", CreatedAt: t0}
				if _, err := s.CreateOutbox(t.Context(), o); err != nil {
					t.Errorf("CreateOutbox: %v", err)
				}
				sess := store.Session{Key: store.ConversationKey{Connection: "c", Channel: "C", Thread: id}, Binding: "ns/b", LastActivity: t0}
				if err := s.UpsertSession(t.Context(), sess); err != nil {
					t.Errorf("UpsertSession: %v", err)
				}
			}
		})
		unposted, err := s.ListUnposted(t.Context())
		if err != nil || len(unposted) != workers*perWorker {
			t.Errorf("unposted outbox rows = %d, %v; want %d", len(unposted), err, workers*perWorker)
		}
		for w := range workers {
			for i := range perWorker {
				id := fmt.Sprintf("w%d-%d", w, i)
				if _, err := s.GetEvent(t.Context(), id); err != nil {
					t.Errorf("event %s: %v", id, err)
				}
				if _, err := s.GetSession(t.Context(), store.ConversationKey{Connection: "c", Channel: "C", Thread: id}, ttl, t0); err != nil {
					t.Errorf("session %s: %v", id, err)
				}
			}
		}
	})
}

func (su suite) sessionTasks(t *testing.T) {
	// SetSessionTask changes the task and the contextId and nothing else: least of all LastActivity,
	// which a concurrent TouchSession may just have moved.
	su.run(t, "SetSessionTask changes the task and the context and nothing else", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		isErr(t, s.SetSessionTask(ctx, key, "c1", "t1", store.TaskTurn{}), store.ErrNotFound, "set on a missing session")
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		for _, tc := range []struct{ name, contextID, taskID, wantContext, wantTask string }{
			{"a task and its context", "c1", "t1", "c1", "t1"},
			{"a later task, no new context", "", "t2", "c1", "t2"},
			{"the task ends", "", "", "c1", ""},
			{"a new context", "c2", "t3", "c2", "t3"},
		} {
			noErr(t, s.TouchSession(ctx, key, t0.Add(time.Minute)), "TouchSession")
			noErr(t, s.SetSessionTask(ctx, key, tc.contextID, tc.taskID, store.TaskTurn{}), tc.name)
			if got := getSession(t, s, key); got.ContextID != tc.wantContext || got.TaskID != tc.wantTask || got.Binding != "ns/b" || !got.LastActivity.Equal(t0.Add(time.Minute)) {
				t.Errorf("%s: session = %+v; want context %q, task %q, the Binding and the touched activity kept", tc.name, got, tc.wantContext, tc.wantTask)
			}
		}
		for range 2 { // a repeat of the same call is not an error, though no row changes
			if err := s.SetSessionTask(ctx, key, "c2", "t3", store.TaskTurn{}); err != nil {
				t.Errorf("repeated set = %v; want nil", err)
			}
		}
		isErr(t, s.SetSessionTask(ctx, store.ConversationKey{}, "c", "t", store.TaskTurn{}), store.ErrInvalid, "set with an empty key")
	})

	// The turn is recorded with the task, in the same write, so that a recovery that finds the task again knows whose
	// output it is: the turn that began it, and after an answer the answer's.
	su.run(t, "SetSessionTask records the turn with the task, replaces it with the next, and clears it with the task", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		if got := getSession(t, s, key); got.Turn != (store.TaskTurn{}) {
			t.Fatalf("a new session's turn = %+v; want none", got.Turn)
		}
		first, answer := store.TaskTurn{ID: "slack:c:C1:1.1", User: "U1"}, store.TaskTurn{ID: "answer-1", User: "U2"}
		noErr(t, s.SetSessionTask(ctx, key, "c1", "t1", first), "the task begins")
		if got := getSession(t, s, key); got.TaskID != "t1" || got.Turn != first || got.TurnTask != "t1" {
			t.Errorf("session = %+v; want task t1 of turn %+v, recorded for t1", got, first)
		}
		noErr(t, s.TouchSession(ctx, key, t0.Add(time.Minute)), "TouchSession")
		if got := getSession(t, s, key); got.Turn != first {
			t.Errorf("a touch changed the turn to %+v", got.Turn)
		}
		active, err := s.ListActiveSessions(ctx)
		if err != nil || len(active) != 1 || active[0].Turn != first {
			t.Errorf("ListActiveSessions = %+v, %v; want the session with its turn %+v", active, err, first)
		}
		// A binary from before the turn was recorded starts another task and leaves the turn: UpsertSession, which replaces the
		// task and not the turn, is that write. The turn then says which task it was recorded for, and it is not this one's.
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, ContextID: "c1", TaskID: "t2", Binding: "ns/b", LastActivity: t0}), "a task that a binary without turns began")
		if got := getSession(t, s, key); got.TaskID != "t2" || got.Turn != first || got.TurnTask != "t1" {
			t.Errorf("session = %+v; want task t2 with the turn %+v still recorded for t1", got, first)
		}
		noErr(t, s.SetSessionTask(ctx, key, "", "t1", first), "back to t1")
		noErr(t, s.SetSessionTask(ctx, key, "", "t1", answer), "the answer continues the task")
		if got := getSession(t, s, key); got.TaskID != "t1" || got.Turn != answer || got.TurnTask != "t1" {
			t.Errorf("session = %+v; want task t1 of the answer %+v: a later call replaces the turn", got, answer)
		}
		noErr(t, s.SetSessionTask(ctx, key, "", "", answer), "the task ends")
		if got := getSession(t, s, key); got.TaskID != "" || got.Turn != (store.TaskTurn{}) || got.TurnTask != "" {
			t.Errorf("session = %+v; want no task and no turn: a turn without a task is cleared whatever is given", got)
		}
		noErr(t, s.SetSessionTask(ctx, key, "", "t2", first), "another task")
		noErr(t, s.ResetSession(ctx, key), "ResetSession")
		if got := getSession(t, s, key); got.TaskID != "" || got.Turn != (store.TaskTurn{}) {
			t.Errorf("session after a reset = %+v; want no task and no turn", got)
		}
	})

	// ResetSession forgets the agent's session, and only that: the Binding and the activity stay, so the
	// thread is still owned, and the next turn starts a fresh context (spec 8.2).
	su.run(t, "ResetSession clears the context and the task and leaves the thread owned", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		isErr(t, s.ResetSession(ctx, key), store.ErrNotFound, "reset of a missing session")
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, ContextID: "c1", TaskID: "t1", Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		for range 2 { // and again: a repeat changes nothing
			noErr(t, s.ResetSession(ctx, key), "ResetSession")
			want := store.Session{Key: key, Binding: "ns/b", LastActivity: t0}
			if got := getSession(t, s, key); !sameSession(got, want) {
				t.Fatalf("session = %+v; want %+v", got, want)
			}
		}
		isErr(t, s.ResetSession(ctx, store.ConversationKey{}), store.ErrInvalid, "reset with an empty key")
	})

	// A SetSessionTask that read the row, changed it and wrote it back whole would undo a touch that
	// ran between the read and the write.
	su.run(t, "SetSessionTask does not undo a touch that runs at the same time", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		const n = 32
		parallel(2*n, func(i int) {
			if i%2 == 0 {
				if err := s.TouchSession(ctx, key, t0.Add(time.Duration(i+1)*time.Minute)); err != nil {
					t.Errorf("touch: %v", err)
				}
			} else if err := s.SetSessionTask(ctx, key, "", fmt.Sprint("t", i), store.TaskTurn{}); err != nil {
				t.Errorf("set task: %v", err)
			}
		})
		got := getSession(t, s, key)
		if got.LastActivity.Equal(t0) || got.TaskID == "" || got.Binding != "ns/b" {
			t.Errorf("session = %+v; want a touched activity and a task", got)
		}
		if first := t0.Add(time.Minute); got.LastActivity.Before(first) || got.LastActivity.After(t0.Add(2*n*time.Minute)) {
			t.Errorf("activity = %v; want one of the touched times", got.LastActivity)
		}
	})
}

func (su suite) recovery(t *testing.T) {
	su.run(t, "ListActiveSessions lists the sessions with a task, oldest activity first, and writes nothing", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		if got, err := s.ListActiveSessions(ctx); err != nil || len(got) != 0 {
			t.Fatalf("empty store = %v, %v; want none", got, err)
		}
		mk := func(thread, taskID string, age time.Duration) store.Session {
			return store.Session{Key: keyOf(thread), ContextID: "ctx-" + thread, TaskID: taskID, Binding: "ns/b", LastActivity: t0.Add(-age)}
		}
		// "old" is far past any TTL: an expired session with an active task is still listed, and the
		// caller decides what to do with it.
		for _, sess := range []store.Session{mk("3.3", "task-c", time.Hour), mk("1.1", "task-a", 2*time.Hour), mk("idle", "", 0), mk("old", "task-o", 4000*time.Hour)} {
			noErr(t, s.UpsertSession(ctx, sess), "upsert "+sess.Key.Thread)
		}
		before := getSession(t, s, keyOf("3.3"))
		got, err := s.ListActiveSessions(ctx)
		noErr(t, err, "ListActiveSessions")
		var threads, tasks []string
		for _, sess := range got {
			threads, tasks = append(threads, sess.Key.Thread), append(tasks, sess.TaskID)
			if sess.Binding != "ns/b" || sess.ContextID != "ctx-"+sess.Key.Thread || sess.LastActivity.IsZero() {
				t.Errorf("session %+v lost a field", sess)
			}
		}
		if want := []string{"old", "1.1", "3.3"}; !slices.Equal(threads, want) { // oldest activity first
			t.Fatalf("threads = %v; want %v", threads, want)
		}
		if want := []string{"task-o", "task-a", "task-c"}; !slices.Equal(tasks, want) {
			t.Fatalf("tasks = %v; want %v", tasks, want)
		}
		if after := getSession(t, s, keyOf("3.3")); !after.LastActivity.Equal(before.LastActivity) {
			t.Fatalf("listing changed LastActivity: %v -> %v", before.LastActivity, after.LastActivity)
		}
	})

	// Sessions with the same activity may come in any order: recovery starts one goroutine for each.
	su.run(t, "sessions with the same activity are all listed, after earlier ones and before later ones", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		ties := []store.ConversationKey{
			{Connection: "slack", Channel: "C1", Thread: "b"},
			{Connection: "slack", Channel: "C1", Thread: "B"},
			{Connection: "slack", Channel: "C0", Thread: "z"},
			{Connection: "Slack", Channel: "C9", Thread: "z"},
		}
		early, late := keyOf("early"), keyOf("late")
		for k, at := range map[store.ConversationKey]time.Time{early: t0.Add(-time.Hour), late: t0.Add(time.Hour)} {
			noErr(t, s.UpsertSession(ctx, store.Session{Key: k, TaskID: "t", Binding: "ns/b", LastActivity: at}), "upsert")
		}
		for _, k := range ties {
			noErr(t, s.UpsertSession(ctx, store.Session{Key: k, TaskID: "t", Binding: "ns/b", LastActivity: t0}), "upsert")
		}
		got, err := s.ListActiveSessions(ctx)
		noErr(t, err, "ListActiveSessions")
		var listed []string
		for _, sess := range got {
			listed = append(listed, keyID(sess.Key))
		}
		if !inTieGroups(listed, []string{keyID(early)}, keyIDs(ties), []string{keyID(late)}) {
			t.Errorf("keys = %q; want early, then the four with one activity in any order, then late", listed)
		}
	})

	// Courier needs each key once; the order of the keys is nobody's.
	su.run(t, "ListQueuedSessions lists each session with an undelivered reply once, in any order", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		if got, err := s.ListQueuedSessions(ctx); err != nil || len(got) != 0 {
			t.Fatalf("empty store = %v, %v; want none", got, err)
		}
		keys := func() []string {
			t.Helper()
			got, err := s.ListQueuedSessions(ctx)
			noErr(t, err, "ListQueuedSessions")
			var ids []string
			for _, k := range got {
				ids = append(ids, keyID(k))
			}
			slices.Sort(ids)
			return ids
		}
		for _, r := range []struct{ id, thread string }{{"b1", "b"}, {"a1", "a"}, {"b2", "b"}, {"a2", "a"}, {"d1", "delivered"}} {
			if _, err := s.EnqueueReply(ctx, newReply(r.id, keyOf(r.thread))); err != nil {
				t.Fatalf("enqueue %s: %v", r.id, err)
			}
		}
		noErr(t, s.MarkReplyDelivered(ctx, "d1", t0), "mark")
		if got, want := keys(), keyIDs([]store.ConversationKey{keyOf("a"), keyOf("b")}); !slices.Equal(got, want) {
			t.Fatalf("keys = %q; want %q: each once, and none that has only tombstones", got, want)
		}
		// A session whose head was delivered is queued while a later reply waits; one with none waiting is not.
		noErr(t, s.MarkReplyDelivered(ctx, "a1", t0), "mark a1")
		for _, id := range []string{"b1", "b2"} {
			noErr(t, s.MarkReplyDelivered(ctx, id, t0), "mark "+id)
		}
		if got, want := keys(), keyIDs([]store.ConversationKey{keyOf("a")}); !slices.Equal(got, want) {
			t.Fatalf("keys after delivering b's replies and a's first = %q; want %q", got, want)
		}
	})

	su.runReopen(t, "the recovery lists survive a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		ctx := t.Context()
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, ContextID: "c", TaskID: "t", Binding: "ns/b", LastActivity: t0}), "upsert")
		_, err := s.EnqueueReply(ctx, newReply("r1", key))
		noErr(t, err, "enqueue")
		s = reopen()
		if got, err := s.ListActiveSessions(ctx); err != nil || len(got) != 1 || got[0].TaskID != "t" {
			t.Fatalf("active after reopen = %+v, %v", got, err)
		}
		if got, err := s.ListQueuedSessions(ctx); err != nil || !slices.Equal(got, []store.ConversationKey{key}) {
			t.Fatalf("queued after reopen = %v, %v", got, err)
		}
	})
}
