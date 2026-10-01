package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Session answers and cancels a paused task: *session.Manager.
type Session interface {
	Answer(ctx context.Context, key store.ConversationKey, msg *a2a.Message) error
	Cancel(ctx context.Context, key store.ConversationKey, taskID a2a.TaskID, reason string) error
}

// resend sends the stored answer of an answered interaction whose pause has come again. The answer is
// built from the stored request, the question the person saw, never from the pause just parsed. A task
// that has moved on is not paused, and the answer is dropped.
func (i *Intake) resend(ctx context.Context, p Pause, b Binding, row store.Interaction) error {
	var in dialect.Interaction
	var answer dialect.Answer
	if err := errors.Join(json.Unmarshal(row.Request, &in), json.Unmarshal(row.Answer, &answer)); err != nil {
		return fmt.Errorf("interact: the stored request or answer of %s: %w", row.ID, err)
	}
	msg, err := AnswerMessage(b.Dialect, row.ID, in, answer, row.Decider)
	if err != nil {
		return fmt.Errorf("interact: rebuild the answer of %s: %w", row.ID, err)
	}
	switch err := i.cfg.Session.Answer(ctx, p.Key, &msg); {
	case errors.Is(err, session.ErrNotPaused):
		i.cfg.Logger.InfoContext(ctx, "interact: dropped an answer whose task has moved on", "interaction", row.ID, "task", row.TaskID)
	case err != nil:
		return fmt.Errorf("interact: send the answer of %s again: %w", row.ID, err)
	}
	return nil
}

// AnswerMessage builds the message that sends an interaction's answer (the stored answer is the
// dialect.Answer as JSON): the dialect's, with a message ID fixed by the interaction, so sending it
// again is the same message, and the person who decided as its attribution. What kagent does with a
// repeated message ID on an answer is UNCONFIRMED(WP-02).
func AnswerMessage(d dialect.Dialect, id string, in dialect.Interaction, answer dialect.Answer, decider string) (a2a.Message, error) {
	msg, err := d.BuildResponse(in, answer)
	if err != nil {
		return a2a.Message{}, err
	}
	msg.ID = id + "/answer"
	d.Attribute(&msg, dialect.User{ID: decider})
	return msg, nil
}
