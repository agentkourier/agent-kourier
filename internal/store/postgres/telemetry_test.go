package postgres

import (
	"context"
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

// TestStoreSpansRequireAParentAndKeepMetrics: as for the SQLite store, an operation of the store makes a
// span only inside a trace, so that background polling makes no root traces, and always makes its
// duration observation.
func TestStoreSpansRequireAParentAndKeepMetrics(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	base := telemetry.WithObserver(t.Context(), obs)
	s, _ := openFresh(t)
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
				t.Fatalf("store span lost its parent: %+v", span)
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
				t.Fatalf("store duration lost an untraced call: %v", m)
			}
		}
	}
	if series != 3 {
		t.Fatalf("store duration series = %d, want 3", series)
	}
}

// TestBacklogsPoolStatsAndInteractionWait: the gauges the SQLite store registers are registered here
// under the same names, with the pool's statistics, and the wait of an interaction is observed once,
// by the call that decided it.
func TestBacklogsPoolStatsAndInteractionWait(t *testing.T) {
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := telemetry.WithObserver(t.Context(), obs)
	s, _ := openFresh(t)
	if err := s.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	key := sessionKey("1")
	for _, id := range []string{"pending", "delivered"} {
		r := reply(id, key)
		r.EnqueuedAt = time.Now().Add(-time.Minute)
		if _, err := s.EnqueueReply(ctx, r); err != nil {
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
	if n, err := testutil.GatherAndCount(reg, "agentkourier_reply_queue_depth", "agentkourier_outbox_pending", "agentkourier_store_pool_max_conns", "agentkourier_store_pool_acquires_total"); err != nil || n != 4 {
		t.Fatalf("collectors: %d series, %v; want 4", n, err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		switch f.GetName() {
		case "agentkourier_reply_queue_depth", "agentkourier_outbox_pending":
			if v := f.Metric[0].Gauge.GetValue(); v != 1 {
				t.Errorf("%s = %v; want 1", f.GetName(), v)
			}
		case "agentkourier_store_pool_max_conns":
			if v := f.Metric[0].Gauge.GetValue(); v != defaultMaxConns {
				t.Errorf("%s = %v; want %d", f.GetName(), v, defaultMaxConns)
			}
		}
	}

	row := store.Interaction{ID: "wait", Key: key, TaskID: "t", Kind: store.InteractionAskUser, CreatedAt: t0, Deadline: t0.Add(time.Hour)}
	if _, err := s.CreateInteraction(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitInteraction(ctx, row.ID, []byte("private answer"), "U", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitInteraction(ctx, row.ID, []byte("private answer"), "U", t0.Add(time.Minute)); err == nil {
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
				t.Fatalf("interaction wait = %v; want one observation of 60 s", h)
			}
		}
	}
	if !found {
		t.Fatal("interaction wait missing")
	}
}
