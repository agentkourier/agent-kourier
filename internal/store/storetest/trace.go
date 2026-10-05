package storetest

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

const (
	// Two carriers, as W3C trace headers, that a caller may hand a store.
	carrierA, stateA = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "vendor=kept"
	carrierB, stateB = "00-11111111111111111111111111111111-2222222222222222-01", ""
)

// traceContext is the durable ancestry of work (spec 14): what a store keeps of the W3C trace headers of the
// rows that carry them, so that work picked up after a restart or a failover joins the trace that began it.
func (su suite) traceContext(t *testing.T) {
	// ancestry reports whether the headers, restored, are of the trace.
	ancestry := func(t *testing.T, what, parent, state string, id trace.TraceID) {
		t.Helper()
		got := trace.SpanContextFromContext(telemetry.Restore(context.Background(), parent, state)).TraceID()
		if parent == "" || got != id {
			t.Errorf("%s: trace headers %q restore to trace %v; want %v", what, parent, got, id)
		}
	}

	su.run(t, "a row written inside a trace remembers it, for whoever reads it later", func(t *testing.T, s store.Store) {
		h := telemetrytest.New(t)
		ctx, op := telemetry.Start(h.Ctx, "courier.intake")
		defer op.Finish(nil)
		id := trace.SpanContextFromContext(ctx).TraceID()

		_, err := s.AppendEvent(ctx, newEvent("ev", t0))
		noErr(t, err, "AppendEvent")
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, TaskID: "task", Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		_, err = s.EnqueueReply(ctx, newReply("r", key))
		noErr(t, err, "EnqueueReply")
		row := store.OutboxEntry{ID: "o", EventID: "ev", Role: store.OutboxReply, Channel: "C", CreatedAt: t0, Payload: "{}"}
		_, err = s.CreateOutbox(ctx, row)
		noErr(t, err, "CreateOutbox")
		_, err = s.CreateInteraction(ctx, newInteraction("i", time.Hour))
		noErr(t, err, "CreateInteraction")
		_, err = s.RecordPress(ctx, press("i", "U1", t0), roomy)
		noErr(t, err, "RecordPress")

		// Read back with no trace at all, as a restart does.
		plain := t.Context()
		ev, err := s.GetEvent(plain, "ev")
		noErr(t, err, "GetEvent")
		ancestry(t, "event", ev.TraceParent, ev.TraceState, id)
		claimed, err := s.ClaimEvent(plain, t0, time.Minute)
		noErr(t, err, "ClaimEvent")
		ancestry(t, "claimed event", claimed.TraceParent, claimed.TraceState, id)
		sess, err := s.GetSession(plain, key, ttl, t0)
		noErr(t, err, "GetSession")
		ancestry(t, "session", sess.TraceParent, sess.TraceState, id)
		active, err := s.ListActiveSessions(plain)
		if err != nil || len(active) != 1 {
			t.Fatalf("ListActiveSessions = %+v, %v", active, err)
		}
		ancestry(t, "listed session", active[0].TraceParent, active[0].TraceState, id)
		r, err := s.NextReply(plain, key)
		noErr(t, err, "NextReply")
		ancestry(t, "reply", r.TraceParent, r.TraceState, id)
		posts := unposted(t, s)
		if len(posts) != 1 {
			t.Fatalf("ListUnposted = %+v", posts)
		}
		ancestry(t, "outbox row", posts[0].TraceParent, posts[0].TraceState, id)
		ix := getInteraction(t, s, "i")
		ancestry(t, "interaction", ix.TraceParent, ix.TraceState, id)
		ps, err := s.ListPresses(plain)
		if err != nil || len(ps) != 1 {
			t.Fatalf("ListPresses = %+v, %v", ps, err)
		}
		ancestry(t, "press", ps[0].TraceParent, ps[0].TraceState, id)
	})

	su.run(t, "trace headers a caller gives are stored as given, and win over the trace of the call", func(t *testing.T, s store.Store) {
		h := telemetrytest.New(t)
		ctx, op := telemetry.Start(h.Ctx, "courier.intake")
		defer op.Finish(nil)

		e := newEvent("ev", t0)
		e.TraceParent, e.TraceState = carrierA, stateA
		_, err := s.AppendEvent(ctx, e)
		noErr(t, err, "AppendEvent")
		sess := store.Session{Key: key, Binding: "ns/b", LastActivity: t0, TraceParent: carrierA, TraceState: stateA}
		noErr(t, s.UpsertSession(ctx, sess), "UpsertSession")
		r := newReply("r", key)
		r.TraceParent, r.TraceState = carrierA, stateA
		_, err = s.EnqueueReply(ctx, r)
		noErr(t, err, "EnqueueReply")
		o := store.OutboxEntry{ID: "o", EventID: "ev", Role: store.OutboxReply, Channel: "C", CreatedAt: t0, TraceParent: carrierA, TraceState: stateA}
		_, err = s.CreateOutbox(ctx, o)
		noErr(t, err, "CreateOutbox")
		i := newInteraction("i", time.Hour)
		i.TraceParent, i.TraceState = carrierA, stateA
		_, err = s.CreateInteraction(ctx, i)
		noErr(t, err, "CreateInteraction")
		p := press("i", "U1", t0)
		p.TraceParent, p.TraceState = carrierA, stateA
		_, err = s.RecordPress(ctx, p, roomy)
		noErr(t, err, "RecordPress")

		ev := getEvent(t, s, "ev")
		gs, _ := s.GetSession(ctx, key, ttl, t0)
		nr, _ := s.NextReply(ctx, key)
		gi := getInteraction(t, s, "i")
		ps, _ := s.ListPresses(ctx)
		got := map[string][2]string{
			"event": {ev.TraceParent, ev.TraceState}, "session": {gs.TraceParent, gs.TraceState}, "reply": {nr.TraceParent, nr.TraceState},
			"outbox": {getOutbox(t, s, "o").TraceParent, getOutbox(t, s, "o").TraceState}, "interaction": {gi.TraceParent, gi.TraceState},
		}
		if len(ps) == 1 {
			got["press"] = [2]string{ps[0].TraceParent, ps[0].TraceState}
		}
		if len(got) != 6 {
			t.Fatalf("read back %d of 6 kinds of row", len(got))
		}
		for what, c := range got {
			if c != [2]string{carrierA, stateA} {
				t.Errorf("%s: trace headers = %q, %q; want those the caller gave, %q, %q", what, c[0], c[1], carrierA, stateA)
			}
		}
	})

	// A redelivery has a trace of its own, and a duplicate is not a change: the row keeps the ancestry of the
	// delivery that made it.
	su.run(t, "a duplicate insert keeps the trace headers of the row it did not change", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		e := newEvent("ev", t0)
		e.TraceParent, e.TraceState = carrierA, stateA
		r := newReply("r", key)
		r.TraceParent, r.TraceState = carrierA, stateA
		o := store.OutboxEntry{ID: "o", EventID: "ev", Role: store.OutboxReply, Channel: "C", CreatedAt: t0, TraceParent: carrierA, TraceState: stateA}
		i := newInteraction("i", time.Hour)
		i.TraceParent, i.TraceState = carrierA, stateA
		for range 2 {
			_, err := s.AppendEvent(ctx, e)
			noErr(t, err, "AppendEvent")
			_, err = s.EnqueueReply(ctx, r)
			noErr(t, err, "EnqueueReply")
			_, err = s.CreateOutbox(ctx, o)
			noErr(t, err, "CreateOutbox")
			_, err = s.CreateInteraction(ctx, i)
			noErr(t, err, "CreateInteraction")
			e.TraceParent, e.TraceState = carrierB, stateB // the second delivery's own trace
			r.TraceParent, r.TraceState = carrierB, stateB
			o.TraceParent, o.TraceState = carrierB, stateB
			i.TraceParent, i.TraceState = carrierB, stateB
		}
		nr, _ := s.NextReply(ctx, key)
		for what, c := range map[string][2]string{
			"event": {getEvent(t, s, "ev").TraceParent, getEvent(t, s, "ev").TraceState}, "reply": {nr.TraceParent, nr.TraceState},
			"outbox":      {getOutbox(t, s, "o").TraceParent, getOutbox(t, s, "o").TraceState},
			"interaction": {getInteraction(t, s, "i").TraceParent, getInteraction(t, s, "i").TraceState},
		} {
			if c != [2]string{carrierA, stateA} {
				t.Errorf("%s: trace headers = %q, %q after a duplicate; want the first delivery's", what, c[0], c[1])
			}
		}
	})

	// A session is written again and again by callers that may have no trace, and one of them must not blank
	// the ancestry that another saved.
	su.run(t, "an upsert with no trace keeps the session's saved trace headers, and one with a trace replaces them", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		sess := store.Session{Key: key, Binding: "ns/b", LastActivity: t0, TraceParent: carrierA, TraceState: stateA}
		noErr(t, s.UpsertSession(ctx, sess), "first upsert")
		get := func() store.Session {
			got, err := s.GetSession(ctx, key, ttl, t0)
			noErr(t, err, "GetSession")
			return got
		}
		sess.TraceParent, sess.TraceState, sess.TaskID = "", "", "task-2"
		noErr(t, s.UpsertSession(ctx, sess), "upsert with no trace")
		if got := get(); got.TraceParent != carrierA || got.TraceState != stateA || got.TaskID != "task-2" {
			t.Fatalf("session = %+v; want the saved trace headers and the rest of the upsert", got)
		}
		sess.TraceParent, sess.TraceState = carrierB, stateB
		noErr(t, s.UpsertSession(ctx, sess), "upsert with a trace")
		if got := get(); got.TraceParent != carrierB || got.TraceState != stateB {
			t.Errorf("session trace = %q, %q; want the new headers, %q, %q (the state replaced too, with none)", got.TraceParent, got.TraceState, carrierB, stateB)
		}
	})

	// Implicit in the interface, which says SetSessionTask changes "nothing else": a task that starts inside a
	// trace is the session's current ancestry, and one set outside any trace leaves it.
	su.run(t, "setting a session's task inside a trace records the trace, and outside one leaves it", func(t *testing.T, s store.Store) {
		h := telemetrytest.New(t)
		ctx, op := telemetry.Start(h.Ctx, "courier.turn")
		defer op.Finish(nil)
		id := trace.SpanContextFromContext(ctx).TraceID()
		sess := store.Session{Key: key, Binding: "ns/b", LastActivity: t0, TraceParent: carrierA, TraceState: stateA}
		noErr(t, s.UpsertSession(t.Context(), sess), "upsert")

		noErr(t, s.SetSessionTask(t.Context(), key, "", "task-1"), "SetSessionTask outside a trace")
		if got := getSession(t, s, key); got.TraceParent != carrierA {
			t.Errorf("trace = %q after a task set outside a trace; want it kept", got.TraceParent)
		}
		noErr(t, s.SetSessionTask(ctx, key, "", "task-2"), "SetSessionTask inside a trace")
		got := getSession(t, s, key)
		ancestry(t, "session after a task set inside a trace", got.TraceParent, got.TraceState, id)
		noErr(t, s.ResetSession(t.Context(), key), "ResetSession")
		if after := getSession(t, s, key); after.TraceParent != got.TraceParent {
			t.Errorf("trace = %q after a reset; want it kept", after.TraceParent)
		}
	})
}
