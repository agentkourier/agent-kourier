package interact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Outcome is what became of a pause that a Binding's policy turned away, as the approval-outcome
// metric labels it. The set is closed, so the metric's cardinality is.
type Outcome string

// The outcomes.
const (
	// OutcomeAskUserCancelled: the agent asked a question on a Binding with askUser false.
	OutcomeAskUserCancelled Outcome = "ask_user_cancelled"
)

// The notices are fixed text: nothing the agent wrote reaches the thread through them (spec 8.4, R2).
// They are enqueued before the cancel is made, so they say it is under way, not that it is done.
const (
	noticeUnusable = "The agent paused for input in a way courier cannot use, so courier is cancelling the task."
	noticeAskUser  = "The agent asked a question, but this Binding does not allow questions, so courier is cancelling the task."
)

// Auditor is what the refusals record through: *audit.Writer.
type Auditor interface {
	UnusablePause(ctx context.Context, a audit.Actor, taskID a2a.TaskID, contextID string, cause error) error
	UnusablePauseCancel(ctx context.Context, a audit.Actor, taskID a2a.TaskID, contextID string, cancelErr error) error
	Decision(ctx context.Context, a audit.Actor, d audit.Decision) error
}

// refuseUnusable turns away a pause the dialect cannot use: the thread is told, the task is
// cancelled, and both are audited. cause wraps dialect.ErrUnusablePause.
//
// Once only: the cancel is what ends the pause, so the call that cancels is the call that audits, and a
// pause that comes again finds the task no longer paused (session.ErrNotPaused) and does nothing. A
// failed cancel is audited as such and returned, to be tried again; a crash between a successful cancel
// and its audit rows loses the rows, which is accepted over writing them twice. The notice has a fixed
// outbox ID, so it is one post however often it is enqueued.
func (i *Intake) refuseUnusable(ctx context.Context, p Pause, b Binding, cause error) error {
	a := audit.Actor{Binding: p.Binding, Identity: b.Identity}
	handled, err := i.cancel(ctx, p, "unusable pause", noticeUnusable, "ux-"+short("unusable", string(p.Task.ID)))
	switch {
	case handled:
		return nil
	case err != nil:
		_ = i.cfg.Audit.UnusablePauseCancel(ctx, a, p.Task.ID, p.Task.ContextID, err) // never stands in the retry's way
		return fmt.Errorf("interact: cancel the unusable pause of task %s: %w", p.Task.ID, err)
	}
	return errors.Join(i.cfg.Audit.UnusablePause(ctx, a, p.Task.ID, p.Task.ContextID, cause),
		i.cfg.Audit.UnusablePauseCancel(ctx, a, p.Task.ID, p.Task.ContextID, nil))
}

// refuseAskUser turns away a question on a Binding that does not allow them: the thread is told, the
// task is cancelled, and the decision is audited and counted, once, by the same rule as refuseUnusable.
func (i *Intake) refuseAskUser(ctx context.Context, p Pause, b Binding, in dialect.Interaction) error {
	id := InteractionID(in)
	handled, err := i.cancel(ctx, p, "ask-user not allowed", noticeAskUser, id)
	switch {
	case handled:
		return nil
	case err != nil:
		return fmt.Errorf("interact: cancel the question %s: %w", id, err)
	}
	if i.cfg.OnApprovalOutcome != nil {
		i.cfg.OnApprovalOutcome(p.Binding, OutcomeAskUserCancelled)
	}
	a := audit.Actor{Binding: p.Binding, Identity: b.Identity}
	return i.cfg.Audit.Decision(ctx, a, audit.Decision{InteractionID: id, TaskID: in.TaskID, Result: audit.DecisionAutoDenied})
}

// cancel enqueues the notice, whose outbox ID comes from id, and cancels the task. handled is true
// when the task was already not paused: the refusal was made before, and there is nothing more to do.
func (i *Intake) cancel(ctx context.Context, p Pause, reason, notice, id string) (handled bool, err error) {
	post := outbox.Post{Source: id, Role: store.OutboxNotice, Channel: p.Key.Channel, ThreadTS: p.Key.ThreadTS, Content: slack.Content{Text: notice}}
	if _, err := i.cfg.Outbox.Enqueue(ctx, post); err != nil {
		return false, fmt.Errorf("enqueue the notice: %w", err)
	}
	err = i.cfg.Session.Cancel(ctx, p.Key, p.Task.ID, reason)
	return errors.Is(err, session.ErrNotPaused), err
}

// short hashes fields into a short ID that holds no agent text.
func short(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(f + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
