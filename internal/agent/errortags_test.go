package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
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
// The expectations are written out, not read from the table the outcomes come from.
func TestFinishAgentOutcomesPinned(t *testing.T) {
	for _, tc := range []struct {
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

// Of the causes an error carries, one names the outcome, and which one is fixed. The order is not the audit log's.
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
		{"a cause with no outcome, then the response and the status", fmt.Errorf("%w: %w: %w", ErrNotSent, dialect.ErrResponseTooLarge,
			&dialect.HTTPStatusError{Code: 413}), finished{"response_too_large", true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := finishAgentSpan(t, tc.err); got != tc.want {
				t.Errorf("finishAgent = %+v; want %+v", got, tc.want)
			}
		})
	}
}
