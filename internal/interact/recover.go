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

// resend sends the stored answer of an answered interaction whose pause has come again. A task that
// has moved on is not paused, and the answer is dropped.
func (i *Intake) resend(ctx context.Context, p Pause, b Binding, row store.Interaction, in dialect.Interaction) error {
	var answer dialect.Answer
	if err := json.Unmarshal(row.Answer, &answer); err != nil {
		return fmt.Errorf("interact: the stored answer of %s: %w", row.ID, err)
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
// again is the same message, and the person who decided as its attribution.
func AnswerMessage(d dialect.Dialect, id string, in dialect.Interaction, answer dialect.Answer, decider string) (a2a.Message, error) {
	msg, err := d.BuildResponse(in, answer)
	if err != nil {
		return a2a.Message{}, err
	}
	msg.ID = id + "/answer"
	d.Attribute(&msg, dialect.User{ID: decider})
	return msg, nil
}
