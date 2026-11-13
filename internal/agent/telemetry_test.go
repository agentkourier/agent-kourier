package agent

import (
	"context"
	"errors"
	"fmt"
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

// finished is what finishAgent recorded on its span: the outcome, which is the metric's outcome label too, and
// whether the operation was told as a failure (the span has an error type).
type finished struct {
	outcome string
	failed  bool
}

func finishAgentSpan(t *testing.T, err error) finished {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	obs, oerr := telemetry.New(prometheus.NewRegistry(), provider)
	if oerr != nil {
		t.Fatal(oerr)
	}
	_, op := telemetry.Start(telemetry.WithObserver(t.Context(), obs), "agentkourier.agent.send")
	finishAgent(op, err)
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans; want 1", len(spans))
	}
	var got finished
	for _, attr := range spans[0].Attributes {
		switch attr.Key {
		case "agentkourier.outcome":
			got.outcome = attr.Value.AsString()
		case "error.type":
			got.failed = true
		}
	}
	return got
}

// Every typed cause has the outcome it has always had, and is a failure or not as it always was: the outcome is a
// metric label, so a renamed one would split a series. A cause with no outcome of its own is told by its error class.
// The expectations are written out, not read from finishAgent's switch.
func TestFinishAgentOutcomesPinned(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want finished
	}{
		// These three differ from the audit log, and the two mappings must not be merged: the audit log tags
		// a2a.ErrTaskNotFound task_not_found and a2a.ErrTaskNotCancelable task_not_cancelable, and lists every cause
		// alike, while the metrics record busy, not_found and already_ended with no error. Audit tags are stored rows
		// and these outcomes are label values, so neither set can be renamed to match the other.
		{ErrBusy, finished{"busy", false}},
		{a2a.ErrTaskNotFound, finished{"not_found", false}},
		{a2a.ErrTaskNotCancelable, finished{"already_ended", false}},
		{ErrCredentialsRejected, finished{"credentials_rejected", true}},
		{ErrForbidden, finished{"forbidden", true}},
		{ErrAgentForbidden, finished{"agent_forbidden", true}},
		{ErrOutcomeUnknown, finished{"outcome_unknown", true}},
		{ErrLookupIncomplete, finished{"lookup_incomplete", true}},
		{ErrStreamLost, finished{"stream_lost", true}},
		{ErrAnswerRejected, finished{"answer_rejected", true}},
		// The audit log names these; the metrics tell them by their error class.
		{ErrNotSent, finished{"error", true}},
		{ErrMessageReply, finished{"error", true}},
		{ErrNotReady, finished{"error", true}},
		{ErrCancelFailed, finished{"error", true}},
		{ErrTokenRequired, finished{"error", true}},
		{ErrTokenUnreadable, finished{"error", true}},
		{ErrHeaderUnreadable, finished{"error", true}},
		{ErrPoolClosed, finished{"error", true}},
		{ErrStreamingUnsupported, finished{"error", true}},
		{dialect.ErrCardTooLarge, finished{"card_too_large", true}},
		{context.Canceled, finished{"cancelled", true}},
		{context.DeadlineExceeded, finished{"timeout", true}},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			for _, err := range []error{tc.err, fmt.Errorf("agent ns/a: %w", tc.err)} {
				if got := finishAgentSpan(t, err); got != tc.want {
					t.Errorf("finishAgent(%v) = %+v; want %+v", err, got, tc.want)
				}
			}
		})
	}
}

// Of the causes an error carries, one names the outcome, and which one is fixed. A card 403 on a reconnect comes as
// a lost stream too, and is told as the 403. The order is not the audit log's, which lists every cause.
func TestFinishAgentOutcomeChoicePinned(t *testing.T) {
	every := errors.Join(ErrCredentialsRejected, ErrNotSent, ErrOutcomeUnknown, ErrMessageReply, ErrStreamLost,
		ErrNotReady, ErrBusy, ErrForbidden, ErrAgentForbidden, ErrAnswerRejected, ErrCancelFailed, ErrTokenRequired,
		ErrTokenUnreadable, ErrHeaderUnreadable, ErrPoolClosed, ErrStreamingUnsupported, dialect.ErrCardTooLarge,
		ErrLookupIncomplete, a2a.ErrTaskNotCancelable, a2a.ErrTaskNotFound, context.Canceled, context.DeadlineExceeded)
	for _, tc := range []struct {
		name string
		err  error
		want finished
	}{
		{"no error", nil, finished{"success", false}},
		{"no typed cause", errors.New("boom"), finished{"error", true}},
		{"every typed cause", every, finished{"busy", false}},
		{"card 403 on a reconnect", fmt.Errorf("%w: %w", ErrStreamLost, ErrAgentForbidden), finished{"agent_forbidden", true}},
		{"answer rejected, its cancel not cancelable", errors.Join(ErrAnswerRejected, a2a.ErrTaskNotCancelable), finished{"already_ended", false}},
		{"answer rejected, its task gone", errors.Join(ErrAnswerRejected, a2a.ErrTaskNotFound), finished{"not_found", false}},
		{"answer rejected, its cancel failed", errors.Join(ErrAnswerRejected, ErrCancelFailed), finished{"answer_rejected", true}},
		{"not ready, so busy", fmt.Errorf("x: %w: %w", ErrNotReady, ErrBusy), finished{"busy", false}},
		{"busy and refused", errors.Join(ErrCredentialsRejected, ErrBusy), finished{"busy", false}},
		{"refused, not sent", errors.Join(ErrNotSent, ErrCredentialsRejected), finished{"credentials_rejected", true}},
		{"lookup incomplete on a lost stream", errors.Join(ErrStreamLost, ErrLookupIncomplete), finished{"lookup_incomplete", true}},
		{"lookup incomplete past its deadline", fmt.Errorf("%w: %w", ErrLookupIncomplete, context.DeadlineExceeded), finished{"lookup_incomplete", true}},
		{"not found and not cancelable", errors.Join(a2a.ErrTaskNotCancelable, a2a.ErrTaskNotFound), finished{"not_found", false}},
		{"outcome unknown and forbidden", errors.Join(ErrOutcomeUnknown, ErrForbidden), finished{"forbidden", true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := finishAgentSpan(t, tc.err); got != tc.want {
				t.Errorf("finishAgent = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// finishAgent names the outcome after the first of its cases that the error matches, so the order of every typed
// cause is fixed: with all of them joined the first names the outcome, with that one dropped the next does, and so
// on down to an error with none. The causes are joined in reverse, so the outcome cannot come from the join order.
// The audit log does not choose: it lists all causes, in its own order.
func TestFinishAgentPrecedencePinned(t *testing.T) {
	precedence := []struct {
		err  error
		want finished
	}{
		{ErrBusy, finished{"busy", false}},
		{a2a.ErrTaskNotFound, finished{"not_found", false}},
		{a2a.ErrTaskNotCancelable, finished{"already_ended", false}},
		{ErrCredentialsRejected, finished{"credentials_rejected", true}},
		{ErrForbidden, finished{"forbidden", true}},
		{ErrAgentForbidden, finished{"agent_forbidden", true}},
		{ErrOutcomeUnknown, finished{"outcome_unknown", true}},
		{ErrLookupIncomplete, finished{"lookup_incomplete", true}},
		{ErrStreamLost, finished{"stream_lost", true}},
		{ErrAnswerRejected, finished{"answer_rejected", true}},
	}
	for i := range len(precedence) + 1 {
		causes := []error{errors.New("boom")}
		for j := len(precedence) - 1; j >= i; j-- {
			causes = append(causes, precedence[j].err)
		}
		want := finished{"error", true}
		if i < len(precedence) {
			want = precedence[i].want
		}
		if got := finishAgentSpan(t, errors.Join(causes...)); got != want {
			t.Errorf("finishAgent(%v) = %+v; want %+v", causes[1:], got, want)
		}
	}
}

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
			_, op := telemetry.Start(telemetry.WithObserver(t.Context(), obs), "agentkourier.agent."+operation)
			finishAgentTask(op, nil, a2a.TaskStateAuthRequired)
			spans := exporter.GetSpans()
			if len(spans) != 1 || spans[0].Status.Code != codes.Unset {
				t.Fatalf("auth-required pause marked as failure: %+v", spans)
			}
			outcome := ""
			for _, attr := range spans[0].Attributes {
				if attr.Key == "agentkourier.outcome" {
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
				if f.GetName() != "agentkourier_operation_duration_seconds" {
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
	ctx, root := telemetry.Start(base, "agentkourier.turn.attempt")
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
		if s.Name == "agentkourier.agent.send" {
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
		if f.GetName() == "agentkourier_agent_first_text_duration_seconds" {
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
	ctx, op := telemetry.Start(h.Ctx, "agentkourier.agent.send")
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
	if got := h.Span(t, "agentkourier.agent.send"); got.Status.Description != "task_failed" {
		t.Fatalf("a failed task's status = %q; the test must also see the span it checks", got.Status.Description)
	}
}
