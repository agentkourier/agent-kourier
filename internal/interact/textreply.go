package interact

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TextReply decides a durable thread reply with the same authorization and audit as Submit.
func (p *Pipeline) TextReply(ctx context.Context, key store.ConversationKey, reply store.Reply, task a2a.Task) (bool, error) {
	sess, err := p.cfg.Sessions.GetSession(ctx, key, 100*365*24*time.Hour, p.cfg.Now())
	if err != nil {
		return false, err
	}
	b, err := p.cfg.Lookup.Binding(sess.Binding)
	if err != nil {
		return false, err
	}
	id := reply.InteractionID
	if id == "" {
		in, err := b.Dialect.ParseInteraction(task)
		if err != nil {
			return false, err
		}
		if in == nil || in.Kind != dialect.AskUser || len(in.Questions) != 1 || len(in.Questions[0].Choices) != 0 {
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
	if reply.InteractionID == "" && reply.EnqueuedAt.Before(row.CreatedAt) {
		return false, store.ErrReplyNotAnswer
	}
	// A closed question that never had a prompt cannot take an answer. Leave new
	// replies as ordinary turns, and retire any claim left by an older process.
	if row.MessageRef == "" && row.State != store.InteractionPending {
		if reply.InteractionID != "" {
			return true, nil
		}
		return false, store.ErrReplyNotAnswer
	}
	if reply.InteractionID == "" {
		if row.MessageRef == "" {
			return false, nil
		}
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
