package sqlite

import (
	"context"
	"errors"
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
	parent, root := telemetry.Start(base, "agentkourier.intake")
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
		if f.GetName() != "agentkourier_operation_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			if !strings.HasPrefix(m.Label[0].GetValue(), "agentkourier.store.") {
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
	if n, err := testutil.GatherAndCount(reg, "agentkourier_reply_queue_depth", "agentkourier_outbox_pending"); err != nil || n != 2 {
		t.Fatalf("backlog collectors: %d %v", n, err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		switch f.GetName() {
		case "agentkourier_reply_queue_depth", "agentkourier_outbox_pending":
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
		if f.GetName() == "agentkourier_interaction_wait_seconds" {
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
