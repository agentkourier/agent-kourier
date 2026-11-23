package telemetry

import (
	"context"
	"errors"
	"strconv"
)

// StatusOf reads the HTTP status that a backend's error carries, when its SDK returns one for an answer of the API: the
// hook by which Outcome names a failed call by its status. Each backend's SDK has its own error type.
type StatusOf func(err error) (status int, ok bool)

// Outcome is the bounded class of a call's error, the outcome label of agentkourier_<add-on>_operation_duration_seconds:
// success, cancelled, timeout, the HTTP status that statusOf reads (100 to 599), or error. Never the error's text. A
// nil statusOf reads no status.
func Outcome(err error, statusOf StatusOf) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case isTimeout(err):
		return "timeout"
	}
	if statusOf != nil {
		if status, ok := statusOf(err); ok && status >= 100 && status <= 599 {
			return strconv.Itoa(status)
		}
	}
	return "error"
}

// Observe runs call as one call op to the add-on's backend: timed in agentkourier_<add-on>_operation_duration_seconds
// and traced as agentkourier.<add-on>.<Spec.Calls>.<op>, a child of the span in ctx, both under the Outcome of call's
// error. call gets ctx with the call's span, so what it returns (a stream) may outlive the span, which ends when call
// returns. A nil t observes nothing.
func Observe[T any](ctx context.Context, t *Telemetry, op Operation, statusOf StatusOf, call func(context.Context) (T, error)) (T, error) {
	ctx, c := t.StartCall(ctx, op)
	v, err := call(ctx)
	c.End(Outcome(err, statusOf))
	return v, err
}
