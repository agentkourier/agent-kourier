package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// apiError is a backend SDK's error for an answer of its API.
type apiError struct{ status int }

func (e *apiError) Error() string {
	return fmt.Sprintf("api answered %d: secret-body-canary", e.status)
}

// statusOf reads apiError's status, as an add-on's hook reads its SDK's.
func statusOf(err error) (int, bool) {
	var e *apiError
	if errors.As(err, &e) {
		return e.status, true
	}
	return 0, false
}

// timeoutError is a network timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

// An error's class is bounded and never its text: the status is the one the hook reads, when it is an HTTP status.
func TestOutcome(t *testing.T) {
	for _, tc := range []struct {
		err      error
		statusOf StatusOf
		want     string
	}{
		{nil, statusOf, "success"},
		{context.Canceled, statusOf, "cancelled"},
		{fmt.Errorf("send: %w", context.Canceled), statusOf, "cancelled"},
		{context.DeadlineExceeded, statusOf, "timeout"},
		{timeoutError{}, statusOf, "timeout"},
		{&apiError{status: 429}, statusOf, "429"},
		{fmt.Errorf("create: %w", &apiError{status: 503}), statusOf, "503"},
		{&apiError{status: 99}, statusOf, "error"},
		{&apiError{status: 600}, statusOf, "error"},
		{&apiError{status: 429}, nil, "error"},
		{errors.New("boom"), statusOf, "error"},
	} {
		if got := Outcome(tc.err, tc.statusOf); got != tc.want {
			t.Errorf("Outcome(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// Observe runs the call inside its span, returns what it returned, and times and traces it under its outcome.
func TestObserveTimesAndTracesTheCall(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	var inside bool
	v, err := Observe(t.Context(), tel, opSend, statusOf, func(ctx context.Context) (string, error) {
		inside = oteltrace.SpanFromContext(ctx).IsRecording()
		return "sent", nil
	})
	if v != "sent" || err != nil || !inside {
		t.Fatalf("Observe = %q, %v; the call ran in the span: %v", v, err, inside)
	}
	failure := &apiError{status: 503}
	if _, err := Observe(t.Context(), tel, opArchive, statusOf, func(context.Context) (struct{}, error) { return struct{}{}, failure }); !errors.Is(err, failure) {
		t.Fatalf("Observe's error = %v, want the call's", err)
	}
	for series, want := range map[string]float64{
		`agentkourier_demo_operation_duration_seconds_count{operation="send",outcome="success"}`: 1,
		`agentkourier_demo_operation_duration_seconds_count{operation="archive",outcome="503"}`:  1,
	} {
		if got := gauge(t, tel, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		spans[s.Name()] = s
	}
	send, archive := spans["agentkourier.demo.backend.send"], spans["agentkourier.demo.backend.archive"]
	if send == nil || archive == nil {
		t.Fatalf("spans = %v", spans)
	}
	if send.Status().Code != codes.Unset || archive.Status().Code != codes.Error || archive.Status().Description != "503" {
		t.Fatalf("statuses = %v, %v", send.Status(), archive.Status())
	}
}
