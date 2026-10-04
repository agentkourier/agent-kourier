package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func TestPollingStoreSpansRequireParentAndKeepMetrics(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	base := telemetry.WithObserver(t.Context(), obs)
	s := openTemp(t)
	parent, root := telemetry.Start(base, "courier.intake")
	parentSC := trace.SpanContextFromContext(parent)
	carrier, state := telemetry.Capture(parent)
	for i, ctx := range []context.Context{base, parent, telemetry.Restore(base, carrier, state)} {
		if _, err := s.ListDueInteractions(ctx, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListUnposted(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListDecidedInteractions(ctx, t0); err != nil {
			t.Fatal(err)
		}
		spans := exporter.GetSpans()
		if len(spans) != i*3 {
			t.Fatalf("poll phase %d exported %d spans, want %d", i, len(spans), i*3)
		}
		for _, span := range spans {
			if span.Parent.SpanID() != parentSC.SpanID() || span.SpanContext.TraceID() != parentSC.TraceID() {
				t.Fatalf("store span lost parent: %+v", span)
			}
		}
	}
	root.Finish(nil)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, f := range families {
		if f.GetName() != "courier_operation_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			if !strings.HasPrefix(m.Label[0].GetValue(), "courier.store.") {
				continue
			}
			series++
			if m.Histogram.GetSampleCount() != 3 {
				t.Fatalf("store duration lost untraced call: %v", m)
			}
		}
	}
	if series != 3 {
		t.Fatalf("store duration series = %d, want 3", series)
	}
}

func TestTraceContextPersistsAcrossReopenAndDuplicate(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	obs, err := telemetry.New(prometheus.NewRegistry(), provider)
	if err != nil {
		t.Fatal(err)
	}
	base := telemetry.WithObserver(t.Context(), obs)
	ctx, root := telemetry.Start(base, "courier.intake")
	traceID := trace.SpanContextFromContext(ctx).TraceID()
	path := filepath.Join(t.TempDir(), "durable.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "ns/conn", Channel: "C", Thread: "1.0"}
	sess := store.Session{Key: key, Binding: "ns/b", LastActivity: t0}
	if err = s.UpsertSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	reply := store.Reply{ID: "turn-1", Key: key, UserID: "U", MessageRef: "1.1", Text: "private prompt", EnqueuedAt: t0}
	if _, err = s.EnqueueReply(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSessionTask(ctx, key, "context-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	out := store.OutboxEntry{ID: "turn-1/reply", EventID: reply.ID, Role: store.OutboxReply, Channel: "C", CreatedAt: t0, Payload: "private response"}
	if _, err = s.CreateOutbox(ctx, out); err != nil {
		t.Fatal(err)
	}
	interaction := store.Interaction{ID: "pause-1", Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, CreatedAt: t0, Deadline: t0.Add(time.Hour)}
	if _, err = s.CreateInteraction(ctx, interaction); err != nil {
		t.Fatal(err)
	}
	press := store.Press{InteractionID: interaction.ID, User: "U", Channel: "C", MessageRef: "1.2", PressedAt: t0, Inputs: []byte("private input")}
	if _, err = s.RecordPress(ctx, press, store.PressLimits{Total: 10, PerUser: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendEvent(ctx, store.Event{ID: "event-1", Kind: "alert", BindingKey: "ns/b", Payload: []byte("private event"), ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	root.Finish(nil)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(base, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.NextReply(base, key)
	if err != nil {
		t.Fatal(err)
	}
	savedParent := got.TraceParent
	// An idempotent redelivery has a new trace but must retain the original ancestry.
	other, op := telemetry.Start(base, "courier.intake")
	if created, err := s.EnqueueReply(other, reply); err != nil || created {
		t.Fatalf("duplicate=%v %v", created, err)
	}
	op.Finish(nil)
	got, err = s.NextReply(base, key)
	if err != nil || got.TraceParent != savedParent {
		t.Fatal("duplicate overwrote trace context")
	}
	event, err := s.GetEvent(base, "event-1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimEvent(base, t0.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	checks := [][2]string{{got.TraceParent, got.TraceState}, {event.TraceParent, event.TraceState}, {claimed.TraceParent, claimed.TraceState}}
	session, err := s.GetSession(base, key, time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	checks = append(checks, [2]string{session.TraceParent, session.TraceState})
	active, err := s.ListActiveSessions(base)
	if err != nil || len(active) != 1 {
		t.Fatal("active session lost")
	}
	checks = append(checks, [2]string{active[0].TraceParent, active[0].TraceState})
	posts, err := s.ListUnposted(base)
	if err != nil || len(posts) != 1 {
		t.Fatal("outbox lost")
	}
	checks = append(checks, [2]string{posts[0].TraceParent, posts[0].TraceState})
	pauses, err := s.ListDueInteractions(base, t0.Add(time.Hour))
	if err != nil || len(pauses) != 1 {
		t.Fatal("interaction lost")
	}
	checks = append(checks, [2]string{pauses[0].TraceParent, pauses[0].TraceState})
	presses, err := s.ListPresses(base)
	if err != nil || len(presses) != 1 {
		t.Fatal("press lost")
	}
	checks = append(checks, [2]string{presses[0].TraceParent, presses[0].TraceState})
	for _, c := range checks {
		restored := telemetry.Restore(base, c[0], c[1])
		if c[0] == "" || trace.SpanContextFromContext(restored).TraceID() != traceID {
			t.Fatalf("lost durable ancestry: %v", c)
		}
	}
}

func TestUpsertSessionWithoutASpanKeepsTheSavedTraceContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	obs, err := telemetry.New(prometheus.NewRegistry(), provider)
	if err != nil {
		t.Fatal(err)
	}
	base := telemetry.WithObserver(t.Context(), obs)
	// The first caller's trace comes with a trace state, so that the state is saved too.
	first, op := telemetry.Start(telemetry.Restore(base, "00-11111111111111111111111111111111-2222222222222222-01", "vendor=kept"), "courier.intake")
	op.Finish(nil)
	s := openTemp(t)
	key := store.ConversationKey{Connection: "ns/conn", Channel: "C", Thread: "1.0"}
	sess := store.Session{Key: key, Binding: "ns/b", LastActivity: t0}
	if err := s.UpsertSession(first, sess); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetSession(t.Context(), key, time.Hour, t0)
	if err != nil || saved.TraceParent == "" || saved.TraceState != "vendor=kept" {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	// A caller with no span (here, no observer at all) must not blank the carrier it did not write.
	sess.LastActivity = t0.Add(time.Minute)
	sess.TaskID = "task-2"
	if err := s.UpsertSession(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetSession(t.Context(), key, time.Hour, t0.Add(time.Minute))
	if err != nil || after.TaskID != "task-2" {
		t.Fatalf("the rest of the upsert was lost: %+v, %v", after, err)
	}
	if after.TraceParent != saved.TraceParent || after.TraceState != saved.TraceState {
		t.Fatalf("upsert without a span blanked the trace context: %q -> %q", saved.TraceParent, after.TraceParent)
	}
	// One with a span still replaces it.
	second, op := telemetry.Start(base, "courier.intake")
	op.Finish(nil)
	if err := s.UpsertSession(second, sess); err != nil {
		t.Fatal(err)
	}
	if after, err = s.GetSession(t.Context(), key, time.Hour, t0.Add(time.Minute)); err != nil || after.TraceParent == saved.TraceParent || after.TraceParent == "" {
		t.Fatalf("upsert with a span did not replace the trace context: %q, %v", after.TraceParent, err)
	}
}

func TestBacklogsAndFirstInteractionResolution(t *testing.T) {
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := telemetry.WithObserver(t.Context(), obs)
	s := openTemp(t)
	if err := s.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "ns/conn", Channel: "C", Thread: "1"}
	for _, id := range []string{"pending", "delivered"} {
		if _, err := s.EnqueueReply(ctx, store.Reply{ID: id, Key: key, UserID: "U", MessageRef: id, Text: "private", EnqueuedAt: time.Now().Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkReplyDelivered(ctx, "delivered", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pending", "abandoned"} {
		if _, err := s.CreateOutbox(ctx, store.OutboxEntry{ID: id, EventID: id, Role: store.OutboxReply, Channel: "C", CreatedAt: time.Now().Add(-time.Minute), Payload: "private"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AbandonOutbox(ctx, "abandoned", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg, "courier_reply_queue_depth", "courier_outbox_pending"); err != nil || n != 2 {
		t.Fatalf("backlog collectors: %d %v", n, err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		switch f.GetName() {
		case "courier_reply_queue_depth", "courier_outbox_pending":
			if f.Metric[0].Gauge.GetValue() != 1 {
				t.Errorf("%s=%v", f.GetName(), f.Metric[0].Gauge.GetValue())
			}
		}
	}
	row := newInteraction("wait", time.Hour)
	if _, err := s.CreateInteraction(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitInteraction(ctx, row.ID, []byte("private answer"), "U", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitInteraction(ctx, row.ID, []byte("private answer"), "U", t0.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("second decision should conflict")
	}
	families, err = reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "courier_interaction_wait_seconds" {
			found = true
			if h := f.Metric[0].Histogram; h.GetSampleCount() != 1 || h.GetSampleSum() != 60 {
				t.Fatalf("interaction wait=%v", h)
			}
		}
	}
	if !found {
		t.Fatal("interaction wait missing")
	}
}
