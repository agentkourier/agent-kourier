package session

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func tracingFixture(t *testing.T) (*fixture, context.Context, *tracetest.InMemoryExporter, *prometheus.Registry) {
	t.Helper()
	f := newFixture(t)
	exporter := tracetest.NewInMemoryExporter()
	p := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	reg := prometheus.NewRegistry()
	o, err := telemetry.New(reg, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := telemetry.WithObserver(t.Context(), o)
	cfg := f.m.cfg
	cfg.Context = ctx
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	f.m, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f, ctx, exporter, reg
}

func TestRunnerPreservesIntakeTraceAndCancellationLifetime(t *testing.T) {
	f, base, exporter, reg := tracingFixture(t)
	sink := f.live()
	ctx, intake := telemetry.Start(base, "agentkourier.intake")
	id := trace.SpanContextFromContext(ctx).TraceID()
	observed := make(chan trace.TraceID, 1)
	f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
		observed <- trace.SpanContextFromContext(ctx).TraceID()
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.m.Submit(ctx, inbound("1")); err != nil {
		t.Fatal(err)
	}
	intake.Finish(nil)
	waitFinal(t, sink)
	if got := <-observed; got != id {
		t.Fatalf("agent trace=%s, intake=%s", got, id)
	}
	f.retired(t)
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range exporter.GetSpans() {
		if s.Name == "agentkourier.turn.attempt" {
			found = true
			if s.SpanContext.TraceID() != id {
				t.Fatal("turn detached from intake")
			}
		}
	}
	if !found {
		t.Fatal("runner lost its injected observer")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	waits := false
	for _, f := range families {
		if f.GetName() == "agentkourier_sessions_active" && f.Metric[0].Gauge.GetValue() != 0 {
			t.Fatal("active gauge leaked")
		}
		if f.GetName() == "agentkourier_turn_queue_wait_seconds" {
			waits = true
			if f.Metric[0].Histogram.GetSampleCount() != 1 {
				t.Fatal("queue wait counted repeatedly")
			}
		}
	}
	if !waits {
		t.Fatal("queue wait not measured")
	}
}

func TestRecoveredTaskLinksToStoredTrace(t *testing.T) {
	f, base, exporter, _ := tracingFixture(t)
	sink := f.live()
	original, op := telemetry.Start(base, "agentkourier.intake")
	f.seed("ns/b1", 0)
	if err := f.st.SetSessionTask(original, key, "c1", "t1", store.TaskTurn{}); err != nil {
		t.Fatal(err)
	}
	originalID := trace.SpanContextFromContext(original).TraceID()
	op.Finish(nil)
	f.agent.get = working(t0)
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
	if err := f.m.Recover(base); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	f.retired(t)
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range exporter.GetSpans() {
		if s.Name == "agentkourier.turn.attempt" {
			found = true
			if len(s.Links) != 1 || s.Links[0].SpanContext.TraceID() != originalID {
				t.Fatalf("recovered turn links=%v", s.Links)
			}
			if s.SpanContext.TraceID() == originalID {
				t.Fatal("restart should create a linked execution trace")
			}
		}
	}
	if !found {
		t.Fatal("recovered attempt missing")
	}
}
