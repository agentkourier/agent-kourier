package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TextReplies owns pause preparation and durable decisions for queued answers. It uses the same
// authorization and audit as button presses. Ready closes after recovery has made the interactions
// available; waiting for it is on the runner, never on the chat acknowledgement path.
type TextReplies struct {
	pipeline *Pipeline
	pauses   PauseHandler
	ready    <-chan struct{}
}

// NewTextReplies builds the queued-answer module; all collaborators are required.
func NewTextReplies(pipeline *Pipeline, pauses PauseHandler, ready <-chan struct{}) (*TextReplies, error) {
	if pipeline == nil || pauses == nil || ready == nil {
		return nil, errors.New("interact: Pipeline, Pauses and Ready are required for text replies")
	}
	return &TextReplies{pipeline: pipeline, pauses: pauses, ready: ready}, nil
}

// Decide prepares an unpinned reply's pause before judging it. A pinned reply belongs to its
// recorded question and never takes up the current task. Preparation and classification share
// the session facts read here; the press pipeline still performs its own authorization checks.
// The task may predate Ready: Paused and the decider re-read durable question state, and
// Manager.Answer verifies that the same task is still parked before accepting an answer.
func (r *TextReplies) Decide(ctx context.Context, key store.ConversationKey, reply store.Reply, task a2a.Task) (session.TextReplyOutcome, error) {
	select {
	case <-ctx.Done():
		return session.TextReplyRetry, ctx.Err()
	case <-r.ready:
	}
	p := r.pipeline
	sess, err := p.cfg.Sessions.GetSession(ctx, key, readAll, p.cfg.Now())
	if err != nil {
		return session.TextReplyRetry, err
	}
	b, err := p.cfg.Lookup.Binding(sess.Binding)
	if err != nil {
		return session.TextReplyRetry, err
	}
	if session.Queued(reply).Kind == session.AdmitTurn {
		// Forced take-up is idempotent with the tap's asynchronous handoff. In particular it
		// refuses and cancels an unusable pause before the reply is left for a later turn.
		if err := r.pauses.Paused(ctx, Pause{Key: key, Binding: sess.Binding, Task: task}); err != nil {
			return session.TextReplyRetry, err
		}
	}
	return p.textReply(ctx, key, reply, task, b)
}

// textReply decides the answer only; reply admission owns whether it is an answer at all.
func (p *Pipeline) textReply(ctx context.Context, key store.ConversationKey, reply store.Reply, task a2a.Task, b Binding) (session.TextReplyOutcome, error) {
	queued := session.Queued(reply)
	id := queued.Question // a reply pinned to its question names it; any other is read from the task's
	if id == "" {
		in, err := b.Dialect.ParseInteraction(task)
		if err != nil {
			if errors.Is(err, dialect.ErrUnusablePause) {
				return session.TextReplyNotAnswer, nil
			}
			return session.TextReplyRetry, err
		}
		if !session.IsTextQuestion(in) {
			return session.TextReplyNotAnswer, nil
		}
		id = InteractionID(*in)
	}
	row, err := p.cfg.Store.GetInteraction(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return session.TextReplyWait, nil
	}
	if err != nil {
		return session.TextReplyRetry, err
	}
	if row.Key != key {
		return session.TextReplyRetry, errors.New("question belongs to another conversation")
	}
	switch v := session.JudgeAnswer(reply, session.AskedQuestion{CreatedAt: row.CreatedAt, Posted: row.MessageRef != "", Open: row.State == store.InteractionPending}); v {
	case session.AnswerDecides: // claimed below, then decided
	case session.AnswerNone:
		return session.TextReplyNotAnswer, nil
	case session.AnswerConsumed: // a claim left by an older process, or an answer typed to a question that closed unseen
		return session.TextReplyConsumed, nil
	case session.AnswerWaits:
		return session.TextReplyWait, nil
	default:
		return session.TextReplyRetry, fmt.Errorf("text reply %s: unknown answer verdict %q", reply.ID, v)
	}
	if queued.Kind == session.AdmitTurn {
		if err := p.cfg.Store.ClaimReplyInteraction(ctx, reply.ID, id); err != nil {
			return session.TextReplyRetry, err
		}
	}
	// A replay after durable delivery cannot decide or resend a later question.
	if !row.DeliveredAt.IsZero() && row.Decider == reply.UserID {
		return session.TextReplyConsumed, nil
	}
	press := store.Press{InteractionID: row.ID, User: reply.UserID, Channel: key.Channel, MessageRef: row.MessageRef, PressedAt: reply.EnqueuedAt}
	if row.MessageRef == "" { // a legacy claim still waiting for its prompt
		if p.cfg.Now().Sub(reply.EnqueuedAt) >= p.cfg.GiveUpAfter {
			return textReplyResult(p.GiveUp(ctx, press))
		}
		return session.TextReplyWait, nil
	}
	inputs, err := json.Marshal([]chat.Answer{{Question: 0, Values: []string{reply.Text}}})
	if err != nil {
		return session.TextReplyRetry, err
	}
	press.Inputs = inputs
	result, err := p.Process(ctx, press)
	if result != Done && ctx.Err() == nil && p.cfg.Now().Sub(reply.EnqueuedAt) >= p.cfg.GiveUpAfter {
		return textReplyResult(p.GiveUp(ctx, press))
	}
	if err != nil {
		return session.TextReplyRetry, err
	}
	if result == Done {
		return session.TextReplyConsumed, nil
	}
	return session.TextReplyWait, nil
}

// textReplyResult keeps a failed give-up retryable; consumption happens only after it succeeds.
func textReplyResult(err error) (session.TextReplyOutcome, error) {
	if err != nil {
		return session.TextReplyRetry, err
	}
	return session.TextReplyConsumed, nil
}
