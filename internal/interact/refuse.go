package interact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
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
	// OutcomeAutoDenied: the agent asked to approve tool calls on a Binding with toolApprovals false.
	OutcomeAutoDenied Outcome = "auto_denied"
)

// The notices are fixed text: nothing the agent wrote reaches the thread through them (spec 8.4, R2).
// They are enqueued before the cancel is made, so they say it is under way, not that it is done.
const (
	noticeUnusable = "The agent paused for input in a way courier cannot use, so courier is cancelling the task."
	noticeAskUser  = "The agent asked a question, but this Binding does not allow questions, so courier is cancelling the task."
	noticeDenied   = "The agent asked to run a tool that needs approval, but this Binding does not allow approvals, so courier is denying the request."
	denyReason     = "denied by courier: this Binding does not allow tool approvals"
	autoDecider    = "courier" // who the interaction records as having decided
)

// Auditor is what the package records through: *audit.Writer. The refusals write the unusable pause and
// its cancel; the refusals and the sweep write decisions.
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

// refuseTooLarge turns away a pause whose prompt does not fit in one message, which no retry mends: it
// is refused as unusable, and the interaction, which has no prompt and never will, is closed with the
// task, so that the sweep has no prompt to replace. cause wraps chat.ErrTooLarge.
func (i *Intake) refuseTooLarge(ctx context.Context, p Pause, b Binding, row store.Interaction, cause error) error {
	if err := i.refuseUnusable(ctx, p, b, fmt.Errorf("%w: %w", dialect.ErrUnusablePause, cause)); err != nil {
		return err
	}
	err := i.cfg.Store.ResolveInteraction(ctx, row.ID, store.InteractionCancelled, i.cfg.Now())
	if err != nil && !errors.Is(err, store.ErrConflict) { // a conflict is a close that was made before
		return fmt.Errorf("interact: close %s, which has no prompt: %w", row.ID, err)
	}
	return nil
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
	post := outbox.Post{Source: id, Role: store.OutboxNotice, Channel: p.Key.Channel, Thread: p.Key.Thread, Out: chat.Say(render.Notice(notice, ""))}
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

// autoDeny turns away a tool approval on a Binding that does not allow them: every tool call is denied
// through the dialect, the thread is told, and the denial is audited and counted.
//
// The decision record authorizes the denial, so it is written first and a failed write stops everything
// (spec 8.6 step 7). Then the interaction's first answer is recorded (SubmitInteraction, which only one
// call wins), and that call counts it. The notice is enqueued, and the stored answer sent, in every call
// that finds the interaction answered, because each is idempotent (a fixed outbox ID, and an answer
// with a fixed message ID that is dropped once the task is no longer paused). So a pause that comes
// again after a failed or interrupted send finishes the send and does not audit or count it again; the
// one window is a crash between the audit write and the answer's record, which audits twice rather
// than risk a denial that is not on record.
func (i *Intake) autoDeny(ctx context.Context, p Pause, b Binding, in dialect.Interaction) error {
	row, err := i.record(ctx, p, b, in)
	if errors.Is(err, dialect.ErrUnusablePause) { // a second approval under a seen ID
		return i.refuseUnusable(ctx, p, b, err)
	} else if err != nil {
		return err
	}
	if row.State == store.InteractionPending {
		answer := dialect.Answer{RequestID: in.RequestID}
		for _, t := range in.Tools {
			answer.Decisions = append(answer.Decisions, dialect.Decision{ToolID: t.ID, Approved: false, Reason: denyReason})
		}
		a := audit.Actor{Binding: p.Binding, Identity: b.Identity}
		if err := i.cfg.Audit.Decision(ctx, a, audit.Decision{InteractionID: row.ID, TaskID: in.TaskID, Result: audit.DecisionAutoDenied, Answer: &answer}); err != nil {
			return fmt.Errorf("interact: audit the denial of %s, which was not sent: %w", row.ID, err)
		}
		raw, err := json.Marshal(answer)
		if err != nil {
			return fmt.Errorf("interact: encode the denial of %s: %w", row.ID, err)
		}
		// The denial is decided by policy when the pause arrives, so it is dated then.
		switch err := i.cfg.Store.SubmitInteraction(ctx, row.ID, raw, autoDecider, row.CreatedAt); {
		case err == nil:
			if i.cfg.OnApprovalOutcome != nil {
				i.cfg.OnApprovalOutcome(p.Binding, OutcomeAutoDenied)
			}
		case !errors.Is(err, store.ErrConflict): // a conflict is another call's win, which this one finishes
			return fmt.Errorf("interact: record the denial of %s: %w", row.ID, err)
		}
		if row, err = i.cfg.Store.GetInteraction(ctx, row.ID); err != nil {
			return fmt.Errorf("interact: read %s: %w", row.ID, err)
		}
	}
	if row.State != store.InteractionAnswered {
		return nil
	}
	post := outbox.Post{Source: row.ID, Role: store.OutboxNotice, Channel: p.Key.Channel, Thread: p.Key.Thread, Out: chat.Say(render.Notice(noticeDenied, ""))}
	if _, err := i.cfg.Outbox.Enqueue(ctx, post); err != nil {
		return fmt.Errorf("interact: enqueue the notice of %s: %w", row.ID, err)
	}
	return i.resend(ctx, p, b, row)
}
