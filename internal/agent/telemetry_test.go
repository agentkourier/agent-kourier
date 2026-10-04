package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func TestAuthRequiredTaskTelemetry(t *testing.T) {
	for _, operation := range []string{"send", "answer", "follow"} {
		t.Run(operation, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			defer func() { _ = provider.Shutdown(context.Background()) }()
			reg := prometheus.NewRegistry()
			obs, err := telemetry.New(reg, provider)
			if err != nil {
				t.Fatal(err)
			}
			_, op := telemetry.Start(telemetry.WithObserver(t.Context(), obs), "courier.agent."+operation)
			finishAgentTask(op, nil, a2a.TaskStateAuthRequired)
			spans := exporter.GetSpans()
			if len(spans) != 1 || spans[0].Status.Code != codes.Unset {
				t.Fatalf("auth-required pause marked as failure: %+v", spans)
			}
			outcome := ""
			for _, attr := range spans[0].Attributes {
				if attr.Key == "courier.outcome" {
					outcome = attr.Value.AsString()
				}
				if attr.Key == "error.type" {
					t.Fatal("auth-required pause has an error type")
				}
			}
			if outcome != string(a2a.TaskStateAuthRequired) {
				t.Fatalf("outcome = %q", outcome)
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range families {
				if f.GetName() != "courier_agent_operation_duration_seconds" {
					continue
				}
				found = true
				if len(f.Metric) != 1 || f.Metric[0].Histogram.GetSampleCount() != 1 {
					t.Fatalf("agent duration observations = %v", f.Metric)
				}
				for _, label := range f.Metric[0].Label {
					if label.GetName() == "outcome" && label.GetValue() != string(a2a.TaskStateAuthRequired) {
						t.Fatalf("metric outcome = %q", label.GetValue())
					}
				}
			}
			if !found {
				t.Fatal("agent duration metric missing")
			}
		})
	}
}

func TestBackendReceivesTurnTraceWithoutPayloadTelemetry(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	base := telemetry.WithObserver(t.Context(), obs)
	ctx, root := telemetry.Start(base, "courier.turn.attempt")
	id := trace.SpanContextFromContext(ctx).TraceID()
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		remote := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(call.Header))
		if trace.SpanContextFromContext(remote).TraceID() != id {
			t.Errorf("%s request lost trace context", call.Method)
		}
		sse(t, w, "livestream/01-slow-stream.response.sse", -1)
	})
	const canary = "private-prompt-canary"
	pool := newPool(t)
	backend := pool.Agent(targetFor(fa, "ns/b", func(context.Context) (dialect.Credentials, error) { return dialect.Credentials{UserID: "test"}, nil }))
	if err := backend.Send(ctx, message(canary), nil); err != nil {
		t.Fatal(err)
	}
	root.Finish(nil)
	for _, call := range []rpc{fa.callAt(0), fa.callAt(1)} {
		remote := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(call.Header))
		if trace.SpanContextFromContext(remote).TraceID() != id {
			t.Fatalf("%s lost context", call.Method)
		}
	}
	sendFound := false
	for _, s := range exporter.GetSpans() {
		if s.Name == "courier.agent.send" {
			sendFound = true
		}
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.AsString(), canary) || strings.Contains(a.Value.AsString(), ticks) {
				t.Fatal("payload content leaked into spans")
			}
		}
	}
	if !sendFound {
		t.Fatal("send operation missing")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	firstText := false
	for _, f := range families {
		if f.GetName() == "courier_agent_first_text_duration_seconds" {
			firstText = true
			if f.Metric[0].Histogram.GetSampleCount() != 1 {
				t.Fatal("first text counted more than once")
			}
		}
	}
	if !firstText {
		t.Fatal("first text metric missing")
	}
}
