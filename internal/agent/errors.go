package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// The errors below are matched with errors.Is. Each also carries the agent's own error, so
// errors.Is(err, a2a.ErrUnsupportedOperation) and the like still work.
//
// A send that fails on the connection, not on the agent's answer, says how far it got:
// ErrNotSent (nothing left, retrying is safe), ErrOutcomeUnknown (the agent may have taken
// the message), or ErrStreamLost (the agent named the task; see StreamLostError).
var (
	// ErrNotSent means the message was not accepted: the Source could not give a connection
	// (a configured token could not be read, say), or the backend answered the send with a
	// 4xx status before any event. Retrying is safe, after fixing what the error names: that
	// is inferred (a front door that refuses a request does not pass it on), not measured.
	// A 408, 429 or 503 must be retried with backoff, no sooner than the RetryAfter of the
	// dialect.HTTPStatusError in the chain.
	ErrNotSent = errors.New("agent: the message was not sent")
	// ErrCredentialsRejected is joined to an error whose HTTP status is 401 or 403: the front
	// door refused the Binding's credentials, so the caller re-mints the token (WP-21e).
	// Before any event it comes with ErrNotSent, and the send can be retried after re-minting.
	ErrCredentialsRejected = errors.New("agent: the backend refused the credentials")
	// ErrOutcomeUnknown means the stream was lost, or the backend failed with a 5xx status,
	// before the agent named a task, so the agent may or may not have accepted the message. Resending the same messageId WITHOUT a
	// contextId is deduplicated by kagent (WP-02, measured). One WITH a contextId is not
	// (WP-03, measured): while the task runs it gets -32004, and after the task ends it starts
	// a second full task. So the caller reconciles first, with FindTask on the contextId, and
	// resends only when the turn is absent (spec 8.2).
	ErrOutcomeUnknown = errors.New("agent: the outcome of the send is unknown")
	// ErrBusy means the session already has an active task (JSON-RPC -32004). The message
	// was not recorded. Do not retry at once: keep it queued, and resend it unchanged, with
	// the same messageId, when the active task ends. -32004 also answers every send to a
	// wedged task, so a task that stays busy past a maximum age needs CancelTask (R17). The
	// error names no task. On Send, some task is active, possibly an input-required pause. On
	// Answer it means busy, wedged, or ended (a2a-go answers -32004 to a message to a
	// terminal task too), so GetTask the task: if it has ended, drop the answer and tell the
	// thread; if it is WORKING, apply the age rule.
	ErrBusy = errors.New("agent: the session has an active task")
	// ErrNotReady means the -32004 is no busy session, the backend's dialect says (Config.NotReady), but one that
	// cannot take input yet: it is starting, or in a lifecycle operation, or the agent is not rolled out. The
	// message was not recorded either, and is resent unchanged after a backoff just the same; what differs is
	// that no other turn is ahead of it. The error is ErrBusy too, for a caller that does not tell the two
	// apart, and says only that it is not ready.
	ErrNotReady = errors.New("agent: the session is not ready to take input")
	// ErrForbidden means the agent refused the caller's identity for this session (-31403).
	ErrForbidden = errors.New("agent: caller is not the session's owner")
	// ErrAnswerRejected means the agent failed an answer to an interaction with -32603, or
	// with any code a2a-go does not know, which it reports as -32603. The task stays in
	// WORKING and refuses every message, so Answer cancels it (R17); a cancel that failed
	// is joined to this error.
	ErrAnswerRejected = errors.New("agent: the answer was rejected and the task is wedged")
	// ErrCancelFailed is joined to ErrAnswerRejected when Answer's automatic cancel of the
	// wedged task failed too: the task is still wedged, and every send to its session will get
	// -32004 until something cancels it (R17). The caller keeps the task recorded, and cancels it
	// again.
	ErrCancelFailed = errors.New("agent: the cancel of the wedged task failed")
	// ErrStreamLost means the stream ended before the task reached an end state, and the
	// reconnects did not get it there. The agent had named the task, so a StreamLostError
	// carries its ID and its state can still be read with GetTask.
	ErrStreamLost = errors.New("agent: stream ended before the task did")
)

// StreamLostError is the error a send returns when it lost its stream after the agent named
// the task, and could not resume it: the reconnects ran out, or a reconnect failed for
// another reason (the Source could not give a connection, or the agent refused). It
// satisfies errors.Is(err, ErrStreamLost) and also wraps that cause.
type StreamLostError struct {
	TaskID    a2a.TaskID
	ContextID string
	Err       error // why the last reconnect failed
}

func (e *StreamLostError) Error() string {
	return fmt.Sprintf("%v (task %s): %v", ErrStreamLost, e.TaskID, e.Err)
}

// Unwrap lets errors.Is find ErrStreamLost and the cause.
func (e *StreamLostError) Unwrap() []error { return []error{ErrStreamLost, e.Err} }

// typed maps the agent's JSON-RPC errors to this package's; other errors pass through.
// answer is true when the call was an answer to an interaction.
func typed(err error, answer bool) error {
	switch {
	case errors.Is(err, a2a.ErrUnsupportedOperation):
		return fmt.Errorf("%w: %w", ErrBusy, err)
	case errors.Is(err, a2a.ErrUnauthorized):
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	case answer && errors.Is(err, a2a.ErrInternalError):
		return fmt.Errorf("%w: %w", ErrAnswerRejected, err)
	case httpStatus(err) == http.StatusUnauthorized || httpStatus(err) == http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrCredentialsRejected, err)
	}
	return err
}

// httpStatus returns the HTTP status the backend answered with, or 0 if err carries none.
func httpStatus(err error) int {
	var hs *dialect.HTTPStatusError
	if errors.As(err, &hs) {
		return hs.Code
	}
	return 0
}

// refused reports whether the backend answered with a 4xx status: it refused the request,
// and will refuse it again.
func refused(err error) bool {
	code := httpStatus(err)
	return code >= 400 && code < 500
}

// permanent reports whether a request that got err would get it again: a 4xx, but not a 408
// (the request timed out) or a 429 (slow down), which say try again, not no.
func permanent(err error) bool {
	code := httpStatus(err)
	return refused(err) && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// retryAfter returns the delay the backend asked for in err's HTTP status, or 0.
func retryAfter(err error) time.Duration {
	var hs *dialect.HTTPStatusError
	if errors.As(err, &hs) {
		return hs.RetryAfter
	}
	return 0
}

// beforeAnyEvent types an error that ended a send before the agent named a task: a 4xx
// means the backend refused the request, so nothing was sent; anything else may have got
// through. That includes a 3xx: kagent never redirects and the client follows no redirect,
// so one is a misconfiguration, and the safe reading of it is "unknown".
func beforeAnyEvent(err error) error {
	err = typed(err, false)
	if refused(err) {
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
}

// isProtocol reports whether err is one the agent itself returned, as opposed to a
// dropped connection: the agent said no, the connection did not fail.
func isProtocol(err error) bool {
	var rpcErr *a2a.Error
	return errors.As(err, &rpcErr)
}

// dropped reports whether err is a lost connection, which is worth a reconnect, and not the
// agent's answer or the caller's own cancellation.
func dropped(ctx context.Context, err error) bool { return ctx.Err() == nil && !isProtocol(err) }

// sendError types the agent's error, or the caller's cancellation, that ended a send. A -32004 that the
// dialect says is a session not ready (notReady, which may be nil) is typed ErrNotReady besides ErrBusy.
func sendError(ctx context.Context, err error, answer bool, notReady func(error) bool) error {
	if ctx.Err() == nil {
		typedErr := typed(err, answer)
		if errors.Is(typedErr, ErrBusy) && notReady != nil && notReady(err) {
			typedErr = &notReadyError{err}
		}
		err = typedErr
	}
	return fmt.Errorf("send message: %w", err)
}

// notReadyError is a -32004 that the dialect says is a session not ready. It is ErrNotReady and also ErrBusy and
// the agent's own error, and it says only the first in its text: "the session has an active task" would be false.
type notReadyError struct{ cause error }

func (e *notReadyError) Error() string { return fmt.Sprintf("%v: %v", ErrNotReady, e.cause) }

func (e *notReadyError) Unwrap() []error { return []error{ErrNotReady, ErrBusy, e.cause} }
