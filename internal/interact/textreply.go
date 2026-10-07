package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TextReply decides a durable thread reply with the same authorization and audit as Submit. Whether the reply is an
// answer to the question at all is not its rule: session.IsTextQuestion says whether the question takes text, and
// session.JudgeAnswer, over the question's row, whether this reply answers it, waits for its prompt or stays a turn
// (store.ErrReplyNotAnswer). What is left here is to decide the answer: the question's conversation, membership,
// the claim, the audit and the answer to the agent.
func (p *Pipeline) TextReply(ctx context.Context, key store.ConversationKey, reply store.Reply, task a2a.Task) (bool, error) {
	sess, err := p.cfg.Sessions.GetSession(ctx, key, 100*365*24*time.Hour, p.cfg.Now())
	if err != nil {
		return false, err
	}
	b, err := p.cfg.Lookup.Binding(sess.Binding)
	if err != nil {
		return false, err
	}
	queued := session.Queued(reply)
	id := queued.Question // a reply pinned to its question names it; any other is read from the task's
	if id == "" {
		in, err := b.Dialect.ParseInteraction(task)
		if err != nil {
			return false, err
		}
		if !session.IsTextQuestion(in) {
			return false, errors.New("not a text question")
		}
		id = InteractionID(*in)
	}
	row, err := p.cfg.Store.GetInteraction(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Key != key {
		return false, errors.New("question belongs to another conversation")
	}
	switch v := session.JudgeAnswer(reply, session.AskedQuestion{CreatedAt: row.CreatedAt, Posted: row.MessageRef != "", Open: row.State == store.InteractionPending}); v {
	case session.AnswerDecides: // claimed below, then decided
	case session.AnswerNone:
		return false, store.ErrReplyNotAnswer
	case session.AnswerConsumed: // a claim left by an older process, or an answer typed to a question that closed unseen
		return true, nil
	case session.AnswerWaits:
		return false, nil
	default:
		return false, fmt.Errorf("text reply %s: unknown answer verdict %q", reply.ID, v)
	}
	if queued.Kind == session.AdmitTurn {
		claims, ok := p.cfg.Store.(interface {
			ClaimReplyInteraction(context.Context, string, string) error
		})
		if !ok {
			return false, errors.New("text replies require durable question claims")
		}
		if err := claims.ClaimReplyInteraction(ctx, reply.ID, id); err != nil {
			return false, err
		}
	}
	// A replay after durable delivery cannot decide or resend a later question.
	if !row.DeliveredAt.IsZero() && row.Decider == reply.UserID {
		return true, nil
	}
	press := store.Press{InteractionID: row.ID, User: reply.UserID, Channel: key.Channel, MessageRef: row.MessageRef, PressedAt: reply.EnqueuedAt}
	if row.MessageRef == "" { // a legacy claim still waiting for its prompt
		if p.cfg.Now().Sub(reply.EnqueuedAt) >= p.cfg.GiveUpAfter {
			return true, p.GiveUp(ctx, press)
		}
		return false, nil
	}
	inputs, err := json.Marshal([]chat.Answer{{Question: 0, Values: []string{reply.Text}}})
	if err != nil {
		return false, err
	}
	press.Inputs = inputs
	result, err := p.Process(ctx, press)
	if result != Done && ctx.Err() == nil && p.cfg.Now().Sub(reply.EnqueuedAt) >= p.cfg.GiveUpAfter {
		return true, p.GiveUp(ctx, press)
	}
	return result == Done, err
}
