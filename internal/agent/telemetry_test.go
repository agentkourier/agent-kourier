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
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
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
				if f.GetName() != "courier_operation_duration_seconds" {
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

// TestObservedEventsNeverExportTheirContent feeds the handler every kind of event, with a canary
// in each piece of content, one by one: a handler that records any chunk, call argument,
// response or final message leaks it, even when no test looks for the whole text joined up.
func TestObservedEventsNeverExportTheirContent(t *testing.T) {
	h := telemetrytest.New(t)
	ctx, op := telemetry.Start(h.Ctx, "courier.agent.send")
	var final a2a.TaskState
	var seen []Event
	handler := observeHandler(ctx, "send", func(ev Event) { seen = append(seen, ev) }, &final)
	answer := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("final-canary"))
	events := []Event{
		StateEvent{TaskID: "task-1", ContextID: "ctx-1", State: a2a.TaskStateWorking},
		TextEvent{ArtifactID: "a1", Text: "chunk-canary-1"},
		TextEvent{ArtifactID: "a1", Text: "chunk-canary-1 chunk-canary-2"},
		ToolCallEvent{ArtifactID: "a1", CallID: "c1", Name: "search", Args: map[string]any{"q": "args-canary"}},
		ToolResultEvent{ArtifactID: "a1", CallID: "c1", Name: "search", Response: map[string]any{"r": "response-canary"}},
		FinalEvent{Task: &a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateFailed, Message: answer}}},
	}
	for _, ev := range events {
		handler(ev)
	}
	op.Finish(nil)
	if len(seen) != len(events) || final != a2a.TaskStateFailed {
		t.Fatalf("the wrapped handler saw %d of %d events, final state %q", len(seen), len(events), final)
	}
	h.RequireNone(t, "chunk-canary-1", "chunk-canary-2", "args-canary", "response-canary", "final-canary")
	if got := h.Span(t, "courier.agent.send"); got.Status.Description != "task_failed" {
		t.Fatalf("a failed task's status = %q; the test must also see the span it checks", got.Status.Description)
	}
}
