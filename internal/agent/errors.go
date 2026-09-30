package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The errors below are matched with errors.Is. Each also carries the agent's own error, so
// errors.Is(err, a2a.ErrUnsupportedOperation) and the like still work. An error that is
// none of these means the send's outcome is unknown; resending the same messageId is safe
// (spec 8.2).
var (
	// ErrBusy means the session already has an active task (JSON-RPC -32004). The message
	// was not recorded. Do not retry at once: keep it queued, and resend it unchanged, with
	// the same messageId, when the active task ends. -32004 also answers every send to a
	// wedged task, so a task that stays busy past a maximum age needs CancelTask (R17).
	ErrBusy = errors.New("agent: the session has an active task")
	// ErrForbidden means the agent refused the caller's identity for this session (-31403).
	ErrForbidden = errors.New("agent: caller is not the session's owner")
	// ErrAnswerRejected means the agent failed an answer to an interaction with -32603. The
	// task stays in WORKING and refuses every message, so the caller must CancelTask it (R17).
	ErrAnswerRejected = errors.New("agent: the answer was rejected and the task is wedged")
	// ErrStreamLost means the stream ended before the task reached an end state, and the
	// agent had named the task, so its state can still be read with GetTask.
	ErrStreamLost = errors.New("agent: stream ended before the task did")
)

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
	}
	return err
}

// isProtocol reports whether err is one the agent itself returned, as opposed to a
// dropped connection: the task is not "lost", the agent said no.
func isProtocol(err error) bool {
	var rpcErr *a2a.Error
	return errors.As(err, &rpcErr)
}

// sendError types the error that ended a send. taskID is the task the agent named, if any.
func sendError(ctx context.Context, err error, taskID a2a.TaskID, answer bool) error {
	switch {
	case ctx.Err() == nil && isProtocol(err):
		err = typed(err, answer)
	case ctx.Err() == nil && taskID != "":
		err = fmt.Errorf("%w: %w", ErrStreamLost, err)
	}
	return fmt.Errorf("send message: %w", err)
}
