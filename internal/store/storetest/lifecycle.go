package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// lifecycle is what a store does across its own life: a restart that loses nothing, a Close that can be
// repeated, and a call whose context ends.
func (su suite) lifecycle(t *testing.T) {
	// One row of every durable kind, in the states that recovery has to resume from: a claimed event, a
	// session with an active task, a queued reply and a delivered one, a row posted and rows not, an
	// answered interaction, presses, and the audit log.
	su.runReopen(t, "everything survives a restart, and sequences go on from where they were", func(t *testing.T, s store.Store, reopen func() store.Store) {
		ctx := t.Context()
		traced := newEvent("ev1", t0)
		traced.TraceParent, traced.TraceState = carrierA, stateA
		_, err := s.AppendEvent(ctx, traced)
		noErr(t, err, "AppendEvent")
		appendEvent(t, s, "ev2", t0.Add(time.Second))
		_, err = s.ClaimEvent(ctx, t0, time.Hour)
		noErr(t, err, "ClaimEvent")
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, ContextID: "c", TaskID: "t", Binding: "ns/b", LastActivity: t0, TraceParent: carrierA, TraceState: stateA}), "UpsertSession")
		for _, id := range []string{"r1", "r2"} {
			r := newReply(id, key)
			r.TraceParent, r.TraceState = carrierA, stateA
			_, err := s.EnqueueReply(ctx, r)
			noErr(t, err, "enqueue "+id)
		}
		noErr(t, s.MarkReplyDelivered(ctx, "r1", t0), "mark r1")
		addOutbox(t, s, "unposted", t0, `{"text":"hi"}`)
		addOutbox(t, s, "posted", t0, `{"text":"hi"}`)
		addOutbox(t, s, "abandoned", t0, `{"text":"hi"}`)
		noErr(t, s.RecordMessageRef(ctx, "posted", "5.5"), "RecordMessageRef")
		_, err = s.AbandonOutbox(ctx, "abandoned", t0.Add(time.Minute))
		noErr(t, err, "AbandonOutbox")
		createInteraction(t, s, "i1")
		noErr(t, s.SetInteractionMessageRef(ctx, "i1", "1700.01"), "SetInteractionMessageRef")
		noErr(t, s.SubmitInteraction(ctx, "i1", []byte("yes"), "U1", t0.Add(time.Minute)), "SubmitInteraction")
		noErr(t, s.MarkInteractionDelivered(ctx, "i1", t0.Add(2*time.Minute)), "MarkInteractionDelivered")
		createInteraction(t, s, "i2")
		kept, err := s.RecordPress(ctx, press("i2", "U1", t0), roomy)
		noErr(t, err, "RecordPress")
		gone, err := s.RecordPress(ctx, press("i2", "U2", t0), roomy)
		noErr(t, err, "RecordPress")
		noErr(t, s.DeletePress(ctx, gone), "DeletePress")
		noErr(t, s.AppendAudit(ctx, newAudit("a", t0)), "AppendAudit")
		noErr(t, s.AppendAudit(ctx, newAudit("b", t0)), "AppendAudit")

		events := []store.Event{getEvent(t, s, "ev1"), getEvent(t, s, "ev2")}
		session := getSession(t, s, key)
		outbox := []store.OutboxEntry{getOutbox(t, s, "unposted"), getOutbox(t, s, "posted"), getOutbox(t, s, "abandoned")}
		interactions := []store.Interaction{getInteraction(t, s, "i1"), getInteraction(t, s, "i2")}
		head, err := s.NextReply(ctx, key)
		noErr(t, err, "NextReply")
		audit, err := s.ListAudit(ctx, time.Time{}, 0, 10)
		noErr(t, err, "ListAudit")

		s = reopen()

		for _, want := range events {
			got := getEvent(t, s, want.ID)
			if got.Status != want.Status || got.Attempts != want.Attempts || !got.NextAttemptAt.Equal(want.NextAttemptAt) || !got.ReceivedAt.Equal(want.ReceivedAt) ||
				got.TraceParent != want.TraceParent || got.TraceState != want.TraceState {
				t.Errorf("event after the restart = %+v; want %+v", got, want)
			}
		}
		if got := getSession(t, s, key); !sameSession(got, session) || got.TraceParent != carrierA || got.TraceState != stateA {
			t.Errorf("session after the restart = %+v; want %+v", got, session)
		}
		if got, err := s.NextReply(ctx, key); err != nil || got.ID != head.ID || got.Sequence != head.Sequence || got.Text != head.Text || got.TraceParent != carrierA || got.TraceState != stateA {
			t.Errorf("head of the queue after the restart = %+v, %v; want %+v", got, err, head)
		}
		if ok, err := s.EnqueueReply(ctx, newReply("r1", key)); err != nil || ok {
			t.Errorf("a delivered reply enqueued after the restart = %v, %v; want it a duplicate still", ok, err)
		}
		for _, want := range outbox {
			if got := getOutbox(t, s, want.ID); !sameOutbox(got, want) {
				t.Errorf("outbox row after the restart = %+v; want %+v", got, want)
			}
		}
		if l := unposted(t, s); !slices.Equal(outboxIDs(l), []string{"unposted"}) || l[0].Payload != `{"text":"hi"}` {
			t.Errorf("unposted after the restart = %+v; want the one row, with its payload", l)
		}
		for _, want := range interactions {
			if got := getInteraction(t, s, want.ID); !sameInteraction(got, want) {
				t.Errorf("interaction after the restart = %+v; want %+v", got, want)
			}
		}
		if ps, err := s.ListPresses(ctx); err != nil || !slices.Equal(pressSeqs(ps), []int64{kept}) {
			t.Errorf("presses after the restart = %+v, %v; want the one not deleted", ps, err)
		}
		if got, err := s.ListAudit(ctx, time.Time{}, 0, 10); err != nil || len(got) != len(audit) || !sameAudit(got[0], audit[0]) || !sameAudit(got[1], audit[1]) {
			t.Errorf("audit after the restart = %+v, %v; want %+v", got, err, audit)
		}

		// Numbers that order rows go on after the largest that was given, even one whose row is gone.
		next, err := s.RecordPress(ctx, press("i2", "U3", t0), roomy)
		noErr(t, err, "RecordPress after the restart")
		if next <= gone {
			t.Errorf("press seq after the restart = %d; want above %d, the last given", next, gone)
		}
		_, err = s.EnqueueReply(ctx, newReply("r3", key))
		noErr(t, err, "enqueue after the restart")
		if r, err := s.NextReplyAfter(ctx, key, head.Sequence); err != nil || r.ID != "r3" || r.Sequence <= head.Sequence {
			t.Errorf("reply after the restart = %+v, %v; want r3 after r2", r, err)
		}
		noErr(t, s.AppendAudit(ctx, newAudit("c", t0)), "AppendAudit after the restart")
		if l, err := s.ListAudit(ctx, time.Time{}, audit[1].Sequence, 10); err != nil || len(l) != 1 || l[0].Subject != "c" {
			t.Errorf("audit after the last entry before the restart = %+v, %v; want the new entry", l, err)
		}
	})

	su.run(t, "Close can be called again, and a closed store returns errors", func(t *testing.T, s store.Store) {
		appendEvent(t, s, "ev", t0)
		noErr(t, s.Close(), "Close")
		if err := s.Close(); err != nil {
			t.Errorf("second Close = %v; want nil", err)
		}
		if _, err := s.GetEvent(t.Context(), "ev"); err == nil || isNotFound(err) {
			t.Errorf("GetEvent on a closed store = %v; want an error that is not ErrNotFound", err)
		}
		if _, err := s.AppendEvent(t.Context(), newEvent("after", t0)); err == nil {
			t.Error("AppendEvent on a closed store succeeded")
		}
	})

	// A call whose context has ended is an error, and one that is not ErrNotFound or any other answer about
	// the data; and it changes nothing, so that a claim that was abandoned is not held.
	su.run(t, "a call with an ended context fails and changes nothing", func(t *testing.T, s store.Store) {
		appendEvent(t, s, "ev", t0)
		ended, cancel := context.WithCancel(t.Context())
		cancel()
		canceled := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s with an ended context = %v; want an error wrapping context.Canceled", what, err)
			}
		}
		_, err := s.ClaimEvent(ended, t0, time.Minute)
		canceled("ClaimEvent", err)
		_, err = s.GetEvent(ended, "ev")
		canceled("GetEvent", err)
		_, err = s.AppendEvent(ended, newEvent("new", t0))
		canceled("AppendEvent", err)
		_, err = s.EnqueueReply(ended, newReply("r", key))
		canceled("EnqueueReply", err)
		_, err = s.CreateOutbox(ended, store.OutboxEntry{ID: "o", EventID: "e", Role: store.OutboxRoot, Channel: "C", CreatedAt: t0})
		canceled("CreateOutbox", err)
		canceled("UpsertSession", s.UpsertSession(ended, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}))
		_, err = s.RecordPress(ended, press("ix", "U1", t0), roomy)
		canceled("RecordPress", err)
		canceled("AppendAudit", s.AppendAudit(ended, newAudit("s", t0)))
		_, err = s.ListAudit(ended, time.Time{}, 0, 10)
		canceled("ListAudit", err)

		if _, err := s.GetEvent(t.Context(), "new"); !isNotFound(err) {
			t.Errorf("an event appended with an ended context exists: %v", err)
		}
		if has, _ := s.HasReply(t.Context(), "r"); has {
			t.Error("a reply enqueued with an ended context exists")
		}
		if l := unposted(t, s); len(l) != 0 {
			t.Errorf("unposted = %+v after a create with an ended context", l)
		}
		if l, _ := s.ListPresses(t.Context()); len(l) != 0 {
			t.Errorf("presses = %+v after a record with an ended context", l)
		}
		if l, _ := s.ListAudit(t.Context(), time.Time{}, 0, 10); len(l) != 0 {
			t.Errorf("audit = %+v after an append with an ended context", l)
		}
		got, err := s.ClaimEvent(t.Context(), t0, time.Minute)
		if err != nil || got.ID != "ev" || got.Attempts != 1 {
			t.Errorf("claim after the abandoned one = %+v, %v; want the event at attempt 1: an abandoned claim holds nothing", got, err)
		}
	})
}
