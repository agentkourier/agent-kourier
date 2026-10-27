package storetest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// The retention promises: what a store clears or drops once content has outlived its use (data protection, DP-1). Each
// is a sweep that the broker runs on a timer; the store owns the rule, so that both stores keep the same one.

// settled creates an interaction with a request and brings it to state at at (a time after t0 + 1 h, the deadline of the
// ones that time out), with an answer if it was answered. An answered one is marked delivered at at when delivered is true.
func settled(t *testing.T, s store.Store, id string, state store.InteractionState, at time.Time, delivered bool) {
	t.Helper()
	ctx := t.Context()
	i := newInteraction(id, time.Hour)
	i.Request = []byte(`{"hint":"the agent's question"}`)
	if state == store.InteractionAnswered {
		i.Deadline = at.Add(time.Hour) // an answer is taken before the deadline
	}
	_, err := s.CreateInteraction(ctx, i)
	noErr(t, err, "create "+id)
	noErr(t, s.SetInteractionMessageRef(ctx, id, "1700."+id), "message ref "+id)
	switch state {
	case store.InteractionAnswered:
		noErr(t, s.SubmitInteraction(ctx, id, []byte(`{"answers":[["what a person typed"]]}`), "U1", at), "submit "+id)
		if delivered {
			noErr(t, s.MarkInteractionDelivered(ctx, id, at), "deliver "+id)
		}
	case store.InteractionTimedOut:
		noErr(t, s.ResolveInteraction(ctx, id, state, at), "time out "+id) // at is at or after the deadline
	case store.InteractionCancelled:
		noErr(t, s.ResolveInteraction(ctx, id, state, at), "cancel "+id)
	}
}

func (su suite) retention(t *testing.T) {
	su.interactionRetention(t)
	su.replyRetention(t)
	su.auditRetention(t)
	su.sessionRetention(t)
}

func (su suite) interactionRetention(t *testing.T) {
	const grace, undelivered = 25 * time.Hour, 7 * 24 * time.Hour

	// Every settled state is cleared once its grace has passed, and not a moment before; what identifies the row stays.
	su.run(t, "ClearSettledInteractions clears a settled interaction's request and answer after the grace and keeps the row", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		at := t0.Add(2 * time.Hour) // after every deadline (t0 + 1 h)
		settled(t, s, "answered", store.InteractionAnswered, at, true)
		settled(t, s, "timed", store.InteractionTimedOut, at, false)
		settled(t, s, "cancelled", store.InteractionCancelled, at, false)
		before := map[string]store.Interaction{}
		for _, id := range []string{"answered", "timed", "cancelled"} {
			before[id] = getInteraction(t, s, id)
		}
		if n, err := s.ClearSettledInteractions(ctx, at.Add(grace-time.Microsecond), grace, undelivered); err != nil || n != 0 {
			t.Fatalf("clear a microsecond before the grace = %d, %v; want 0", n, err)
		}
		for id, want := range before {
			if got := getInteraction(t, s, id); !sameInteraction(got, want) {
				t.Fatalf("%s before the grace = %+v; want it untouched", id, got)
			}
		}
		if n, err := s.ClearSettledInteractions(ctx, at.Add(grace), grace, undelivered); err != nil || n != 3 {
			t.Fatalf("clear at the grace = %d, %v; want all 3", n, err)
		}
		for id, was := range before {
			want := was
			want.Request, want.Answer = nil, nil
			got := getInteraction(t, s, id)
			if !sameInteraction(got, want) || len(got.Request) != 0 || len(got.Answer) != 0 {
				t.Errorf("%s after = %+v; want only the request and answer gone: %+v", id, got, want)
			}
		}
		if n, err := s.ClearSettledInteractions(ctx, at.Add(100*grace), grace, undelivered); err != nil || n != 0 {
			t.Errorf("clear again = %d, %v; want 0: a cleared row is not counted twice", n, err)
		}
		// The repair and the backstop list by decision time, and a cleared row is still decided.
		if l, err := s.ListDecidedInteractions(ctx, t0); err != nil || len(l) != 3 {
			t.Errorf("ListDecidedInteractions after = %v, %v; want the 3 rows", interactionIDs(l), err)
		}
	})

	su.run(t, "ClearSettledInteractions leaves a pending interaction alone, however old", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		createInteraction(t, s, "pending") // due at t0 + 1 h, and never decided
		settled(t, s, "other", store.InteractionCancelled, t0, false)
		want := getInteraction(t, s, "pending")
		if n, err := s.ClearSettledInteractions(ctx, t0.Add(1000*grace), grace, undelivered); err != nil || n != 1 {
			t.Fatalf("clear = %d, %v; want only the cancelled one", n, err)
		}
		if got := getInteraction(t, s, "pending"); !sameInteraction(got, want) || len(got.Request) == 0 {
			t.Errorf("pending after = %+v; want it whole: %+v", got, want)
		}
	})

	// An answer that was never marked delivered is the only copy of what the session may still be given.
	su.run(t, "ClearSettledInteractions keeps an answer not yet delivered for the longer grace", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		at := t0.Add(2 * time.Hour)
		settled(t, s, "undelivered", store.InteractionAnswered, at, false)
		settled(t, s, "timed", store.InteractionTimedOut, at, false) // no answer to deliver: the short grace
		if n, err := s.ClearSettledInteractions(ctx, at.Add(grace), grace, undelivered); err != nil || n != 1 {
			t.Fatalf("clear at the short grace = %d, %v; want only the timed-out one", n, err)
		}
		if got := getInteraction(t, s, "undelivered"); len(got.Request) == 0 || len(got.Answer) == 0 {
			t.Fatalf("the undelivered answer after the short grace = %+v; want it kept", got)
		}
		// Delivered since: it no longer waits for the long grace.
		noErr(t, s.MarkInteractionDelivered(ctx, "undelivered", at.Add(grace)), "deliver")
		if n, err := s.ClearSettledInteractions(ctx, at.Add(grace), grace, undelivered); err != nil || n != 1 {
			t.Fatalf("clear once delivered = %d, %v; want it", n, err)
		}
		settled(t, s, "undelivered2", store.InteractionAnswered, at, false)
		if n, err := s.ClearSettledInteractions(ctx, at.Add(undelivered-time.Microsecond), grace, undelivered); err != nil || n != 0 {
			t.Fatalf("clear a microsecond before the long grace = %d, %v; want 0", n, err)
		}
		if n, err := s.ClearSettledInteractions(ctx, at.Add(undelivered), grace, undelivered); err != nil || n != 1 {
			t.Fatalf("clear at the long grace = %d, %v; want the answer, delivered or not", n, err)
		}
	})

	su.run(t, "ClearSettledInteractions refuses arguments that are not legal", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for name, c := range map[string]struct {
			now                  time.Time
			grace, undeliveredBy time.Duration
		}{
			"no time":                    {time.Time{}, grace, undelivered},
			"a negative grace":           {t0, -time.Second, undelivered},
			"a negative undelivered one": {t0, grace, -time.Second},
			"undelivered sooner":         {t0, grace, grace - time.Second},
		} {
			_, err := s.ClearSettledInteractions(ctx, c.now, c.grace, c.undeliveredBy)
			isErr(t, err, store.ErrInvalid, name)
		}
	})
}

func (su suite) replyRetention(t *testing.T) {
	const age = 30 * 24 * time.Hour
	other := keyOf("2.2")
	queued := func(t *testing.T, s store.Store, id string, k store.ConversationKey, at time.Time) {
		t.Helper()
		r := newReply(id, k)
		r.EnqueuedAt = at
		if ok, err := s.EnqueueReply(t.Context(), r); err != nil || !ok {
			t.Fatalf("enqueue %s = %v, %v", id, ok, err)
		}
	}

	su.run(t, "AbandonStrandedReplies drops an undelivered reply older than the age, text cleared, and nothing younger", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		queued(t, s, "old", key, t0)
		queued(t, s, "young", key, t0.Add(time.Hour))
		queued(t, s, "other", other, t0)
		noErr(t, s.MarkReplyDelivered(ctx, "other", t0), "deliver other") // a tombstone: not stranded
		if n, err := s.AbandonStrandedReplies(ctx, t0.Add(age-time.Microsecond), age); err != nil || n != 0 {
			t.Fatalf("abandon a microsecond before the age = %d, %v; want 0", n, err)
		}
		now := t0.Add(age)
		if n, err := s.AbandonStrandedReplies(ctx, now, age); err != nil || n != 1 {
			t.Fatalf("abandon at the age = %d, %v; want only the old one", n, err)
		}
		head, err := s.NextReply(ctx, key)
		if err != nil || head.ID != "young" || head.Text != "hello" {
			t.Fatalf("head after = %+v, %v; want the young reply, whole", head, err)
		}
		if ok, err := s.HasReply(ctx, "old"); err != nil || !ok {
			t.Errorf("HasReply(old) = %v, %v; want the tombstone to stay, so a replayed event is not queued again", ok, err)
		}
		if ok, err := s.EnqueueReply(ctx, newReply("old", key)); err != nil || ok {
			t.Errorf("enqueue of the dropped ID again = %v, %v; want a duplicate", ok, err)
		}
		if n, err := s.AbandonStrandedReplies(ctx, now, age); err != nil || n != 0 {
			t.Errorf("abandon again = %d, %v; want 0", n, err)
		}
		if n, err := s.AbandonStrandedReplies(ctx, t0.Add(time.Hour+age), age); err != nil || n != 1 {
			t.Errorf("abandon the young one once it is old = %d, %v; want 1", n, err)
		}
	})

	// The one wait that is by design: a turn queued behind a task that runs or is paused on a question.
	su.run(t, "AbandonStrandedReplies leaves a reply whose session has an active task", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}), "session")
		noErr(t, s.SetSessionTask(ctx, key, "ctx1", "task1", store.TaskTurn{}), "task")
		queued(t, s, "behind", key, t0)
		queued(t, s, "free", other, t0) // no session at all: nothing will ever send it
		now := t0.Add(10 * age)
		if n, err := s.AbandonStrandedReplies(ctx, now, age); err != nil || n != 1 {
			t.Fatalf("abandon = %d, %v; want only the reply with no session", n, err)
		}
		if got, err := s.NextReply(ctx, key); err != nil || got.ID != "behind" || got.Text != "hello" {
			t.Fatalf("the reply behind the task = %+v, %v; want it whole", got, err)
		}
		noErr(t, s.SetSessionTask(ctx, key, "", "", store.TaskTurn{}), "the task ends")
		if n, err := s.AbandonStrandedReplies(ctx, now, age); err != nil || n != 1 {
			t.Fatalf("abandon once the task has ended = %d, %v; want the reply that waited behind it", n, err)
		}
	})

	su.run(t, "acknowledgment retention lists completed removals and live additions separately", func(t *testing.T, s store.Store) {
		lister, ok := s.(store.AcknowledgmentLister)
		if !ok {
			t.Fatal("store does not implement AcknowledgmentLister")
		}
		pending, ok := s.(store.PendingAcknowledgmentLister)
		if !ok {
			t.Fatal("store does not implement PendingAcknowledgmentLister")
		}
		ctx := t.Context()
		old := newReply("ack-old", key)
		old.EnqueuedAt = t0
		young := newReply("ack-young", key)
		young.EnqueuedAt = t0.Add(time.Hour)
		active := newReply("ack-active", other)
		active.EnqueuedAt = t0
		delivered := newReply("ack-delivered", keyOf("3.3"))
		delivered.EnqueuedAt = t0
		for _, r := range []store.Reply{old, young, active, delivered} {
			if _, err := s.EnqueueReply(ctx, r); err != nil {
				t.Fatalf("enqueue %s: %v", r.ID, err)
			}
		}
		noErr(t, s.MarkReplyDelivered(ctx, delivered.ID, t0.Add(time.Hour)), "deliver acknowledgment reply")
		noErr(t, s.UpsertSession(ctx, store.Session{Key: other, Binding: "ns/b", LastActivity: t0}), "active session")
		noErr(t, s.MarkReplyDelivered(ctx, active.ID, t0.Add(time.Hour)), "deliver active acknowledgment reply")
		noErr(t, s.SetSessionTask(ctx, other, "c1", "task1", store.TaskTurn{ID: active.ID, MessageRef: active.MessageRef, Acknowledgment: active.Acknowledgment}), "active task")

		got, err := lister.ListAcknowledgmentReplies(ctx, t0.Add(age), age)
		noErr(t, err, "list acknowledgment replies")
		ids := make([]string, 0, len(got))
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		if want := []string{"ack-delivered"}; !slices.Equal(ids, want) {
			t.Fatalf("acknowledgment replies = %v; want completed, inactive tombstones only %v", ids, want)
		}

		gotPending, err := pending.ListPendingAcknowledgmentReplies(ctx)
		noErr(t, err, "list pending acknowledgment replies")
		ids = ids[:0]
		for _, r := range gotPending {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		if want := []string{"ack-active", "ack-old", "ack-young"}; !slices.Equal(ids, want) {
			t.Fatalf("pending acknowledgment replies = %v; want undelivered and active sources %v", ids, want)
		}
		for _, tc := range []struct{ name, outputID, sourceID string }{
			{"normal pause", active.ID, ""},
			{"recovered pause with a stale source ID", "question/answer", active.ID},
		} {
			t.Run(tc.name, func(t *testing.T) {
				noErr(t, s.SetSessionTask(ctx, other, "c1", "task1", store.TaskTurn{ID: tc.outputID, MessageRef: active.MessageRef, AcknowledgmentID: tc.sourceID}), "pause without an active acknowledgment")
				removed, err := lister.ListAcknowledgmentReplies(ctx, t0.Add(age), age)
				noErr(t, err, "list paused acknowledgment removals")
				removalIDs := make([]string, 0, len(removed))
				for _, r := range removed {
					removalIDs = append(removalIDs, r.ID)
				}
				slices.Sort(removalIDs)
				if want := []string{"ack-active", "ack-delivered"}; !slices.Equal(removalIDs, want) {
					t.Fatalf("paused removals = %v; want %v", removalIDs, want)
				}
				added, err := pending.ListPendingAcknowledgmentReplies(ctx)
				noErr(t, err, "list paused acknowledgment additions")
				for _, r := range added {
					if r.ID == active.ID {
						t.Fatal("the paused source was scheduled for another acknowledgment add")
					}
				}
			})
		}
	})

	su.run(t, "a settled acknowledgment is listed for removal no more", func(t *testing.T, s store.Store) {
		lister, ok := s.(store.AcknowledgmentLister)
		if !ok {
			t.Fatal("store does not implement AcknowledgmentLister")
		}
		pending, ok := s.(store.PendingAcknowledgmentLister)
		if !ok {
			t.Fatal("store does not implement PendingAcknowledgmentLister")
		}
		ctx := t.Context()
		for _, r := range []store.Reply{newReply("settled", key), newReply("unsettled", other), newReply("queued", keyOf("3.3"))} {
			if _, err := s.EnqueueReply(ctx, r); err != nil {
				t.Fatalf("enqueue %s: %v", r.ID, err)
			}
		}
		noErr(t, s.MarkReplyDelivered(ctx, "settled", t0.Add(time.Hour)), "deliver settled")
		noErr(t, s.MarkReplyDelivered(ctx, "unsettled", t0.Add(time.Hour)), "deliver unsettled")
		listed := func(t *testing.T) []string {
			t.Helper()
			got, err := lister.ListAcknowledgmentReplies(ctx, t0.Add(age), age)
			noErr(t, err, "list acknowledgment replies")
			ids := make([]string, 0, len(got))
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			slices.Sort(ids)
			return ids
		}
		if got, want := listed(t), []string{"settled", "unsettled"}; !slices.Equal(got, want) {
			t.Fatalf("before the settle, acknowledgment replies = %v; want %v", got, want)
		}
		noErr(t, lister.SettleAcknowledgmentReply(ctx, "settled"), "settle")
		noErr(t, lister.SettleAcknowledgmentReply(ctx, "settled"), "settle again") // a repeat changes nothing and succeeds
		if got, want := listed(t), []string{"unsettled"}; !slices.Equal(got, want) {
			t.Fatalf("after the settle, acknowledgment replies = %v; want %v: a settled removal is not queued again", got, want)
		}
		isErr(t, lister.SettleAcknowledgmentReply(ctx, "queued"), store.ErrNotFound, "settle an undelivered reply")
		isErr(t, lister.SettleAcknowledgmentReply(ctx, "nope"), store.ErrNotFound, "settle an unknown reply")
		isErr(t, lister.SettleAcknowledgmentReply(ctx, ""), store.ErrInvalid, "settle no reply")
		got, err := pending.ListPendingAcknowledgmentReplies(ctx)
		noErr(t, err, "list pending acknowledgment replies")
		if len(got) != 1 || got[0].ID != "queued" || got[0].Acknowledgment != "eyes" {
			t.Fatalf("pending acknowledgment replies = %+v; want the undelivered reply, its acknowledgment kept", got)
		}
	})

	su.run(t, "AbandonStrandedReplies refuses arguments that are not legal", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		_, err := s.AbandonStrandedReplies(ctx, time.Time{}, age)
		isErr(t, err, store.ErrInvalid, "no time")
		for _, bad := range []time.Duration{0, -time.Hour} {
			_, err = s.AbandonStrandedReplies(ctx, t0, bad)
			isErr(t, err, store.ErrInvalid, fmt.Sprint("an age of ", bad))
		}
	})
}

func (su suite) auditRetention(t *testing.T) {
	appendAt := func(t *testing.T, s store.Store, subject string, at time.Time) {
		t.Helper()
		noErr(t, s.AppendAudit(t.Context(), newAudit(subject, at)), "append "+subject)
	}
	subjects := func(t *testing.T, s store.Store) []string {
		t.Helper()
		l, err := s.ListAudit(t.Context(), time.Time{}, 0, 1000)
		noErr(t, err, "ListAudit")
		return auditSubjects(l)
	}

	su.run(t, "PurgeAudit deletes the oldest entries before the cutoff, at most the limit, and nothing at or after it", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		// Appended out of time order: it is the entry's time that is retained, not the order it was appended in.
		for i, at := range []time.Duration{3, 1, 0, 2, 4, 1} {
			appendAt(t, s, fmt.Sprint("e", i), t0.Add(at*time.Hour))
		}
		cutoff := t0.Add(3 * time.Hour) // before: e1 (1 h), e2 (0), e3 (2 h), e5 (1 h); not e0 (3 h, the boundary) nor e4
		if n, err := s.PurgeAudit(ctx, t0, 10); err != nil || n != 0 {
			t.Fatalf("purge before the oldest = %d, %v; want 0", n, err)
		}
		if n, err := s.PurgeAudit(ctx, cutoff, 2); err != nil || n != 2 {
			t.Fatalf("purge with a limit of 2 = %d, %v; want 2", n, err)
		}
		left := subjects(t, s)
		if slices.Contains(left, "e2") || !slices.Contains(left, "e3") || len(left) != 4 { // e2 is the oldest; of the 1 h pair one went
			t.Fatalf("after the first batch %v; want the oldest (e2) and one of the two at 1 h gone, e3 kept", left)
		}
		if n, err := s.PurgeAudit(ctx, cutoff, 10); err != nil || n != 2 {
			t.Fatalf("purge the rest before the cutoff = %d, %v; want 2: fewer than the limit says it is done", n, err)
		}
		if left := subjects(t, s); !slices.Equal(left, []string{"e0", "e4"}) {
			t.Fatalf("after the purge %v; want the entries at and after the cutoff, in append order", left)
		}
		if n, err := s.PurgeAudit(ctx, cutoff, 10); err != nil || n != 0 {
			t.Errorf("purge again = %d, %v; want 0", n, err)
		}
		// The log goes on: the sequence is never given again, and listing from a cursor is as before.
		all, _ := s.ListAudit(ctx, time.Time{}, 0, 10)
		appendAt(t, s, "e6", t0.Add(5*time.Hour))
		next, err := s.ListAudit(ctx, time.Time{}, all[len(all)-1].Sequence, 10)
		if err != nil || len(next) != 1 || next[0].Subject != "e6" || next[0].Sequence <= all[len(all)-1].Sequence {
			t.Errorf("an entry appended after a purge = %+v, %v; want e6 with a sequence above the last", next, err)
		}
	})

	su.run(t, "PurgeAudit refuses arguments that are not legal", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendAt(t, s, "a", t0)
		_, err := s.PurgeAudit(ctx, time.Time{}, 10)
		isErr(t, err, store.ErrInvalid, "no cutoff")
		for _, bad := range []int{0, -1} {
			_, err = s.PurgeAudit(ctx, t0.Add(time.Hour), bad)
			isErr(t, err, store.ErrInvalid, fmt.Sprint("a limit of ", bad))
		}
		if got := subjects(t, s); !slices.Equal(got, []string{"a"}) {
			t.Errorf("after refused purges %v; want a", got)
		}
	})
}
