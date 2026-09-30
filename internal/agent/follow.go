package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Follow delivers to h the rest of a task the caller already knows by ID, and returns when
// the task ends or pauses for input (h has then got a FinalEvent) or when following fails.
// It is how the session manager takes a task back after a restart (spec 8.3) and after a
// send lost its stream. It reconciles as Send does after a drop: GetTask, at once, then
// SubscribeToTask if the task has not settled. The text the task holds so far arrives as one
// TextEvent per artifact, and none is delivered twice. A task that has ended, or awaits
// input, is reported with a FinalEvent and no subscription.
//
// Errors:
//   - The agent's own answer comes back at once, typed as for Send: errors.Is(err,
//     a2a.ErrTaskNotFound) when it no longer knows the task (the session may be gone),
//     ErrForbidden when the caller does not own it.
//   - A stream that drops is resumed up to MaxReconnects times in a row, then Follow returns
//     a *StreamLostError carrying the task ID, so the caller may follow again later.
//   - A 401 or 403 also comes back as a *StreamLostError, and matches ErrCredentialsRejected:
//     the caller re-mints the credential (D34).
//
// h may be nil. A task ID is required.
func (a *Agent) Follow(ctx context.Context, id a2a.TaskID, h Handler) error {
	if id == "" {
		return errors.New("follow task: a task ID is required")
	}
	conn, err := a.source(ctx)
	if err != nil {
		return fmt.Errorf("connect to agent: %w", typed(err, false))
	}
	asm := newAssembler(h, a.tool)
	asm.taskID = id
	switch err = a.resume(ctx, conn, asm); {
	case err == nil:
		return nil
	case !dropped(ctx, err):
		if ctx.Err() == nil { // the agent answered; a cancelled caller keeps its own error
			err = typed(err, false)
		}
		return fmt.Errorf("follow task %s: %w", id, err)
	}
	switch err = a.reconnect(ctx, asm, err); {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("follow task %s: %w", id, err)
	}
	return &StreamLostError{TaskID: id, ContextID: asm.contextID, Err: typed(err, false)}
}
