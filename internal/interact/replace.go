package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// ReplacerConfig configures a Replacer. Store and Updater are required.
type ReplacerConfig struct {
	Store   store.Interactions
	Updater Updater
	Logger  *slog.Logger // slog.Default() if nil
}

// Replacer takes the controls off the prompt of an interaction that has ended, and says how it ended
// in their place (spec 8.4 step 5). It is safe for concurrent use.
type Replacer struct{ cfg ReplacerConfig }

// NewReplacer returns a Replacer over cfg.
func NewReplacer(cfg ReplacerConfig) (*Replacer, error) {
	if cfg.Store == nil || cfg.Updater == nil {
		return nil, errors.New("interact: Store and Updater are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Replacer{cfg}, nil
}

// Replace edits the prompt of the interaction id into its replacement, built from the row: who
// decided and when, how it ended, and a summary of the answer, with no input or button. It does
// nothing, and returns nil, for an interaction that is pending, that has no prompt posted, or that is
// gone. The edit is the same however often it is made, so a failed one, or one repeated after a crash,
// is tried again. The error of a failed edit is returned.
func (r *Replacer) Replace(ctx context.Context, id string) error {
	row, err := r.cfg.Store.GetInteraction(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("interact: read %s: %w", id, err)
	case row.State == store.InteractionPending || row.MessageRef == "":
		return nil
	}
	content, err := ReplacementContent(row)
	if err != nil {
		return fmt.Errorf("interact: render the replacement of %s: %w", id, err)
	}
	if err := r.cfg.Updater.Update(ctx, row.Key.Channel, row.MessageRef, content); err != nil {
		return fmt.Errorf("interact: replace the prompt of %s: %w", id, err)
	}
	return nil
}

// ReplacementContent is the message that takes the place of the prompt of a decided interaction. It is
// built from the row alone: the outcome from its state, who from its decider and when from its decision
// time, and the questions, hint and sub-agent from its stored request. An answer is shown only as its
// chosen choices, and never as free text (the owner's decision); that is render's rule, and the answer
// is not otherwise used here.
func ReplacementContent(row store.Interaction) (slack.Content, error) {
	e := render.Ending{By: row.Decider, At: row.DecidedAt}
	switch row.State {
	case store.InteractionAnswered:
		e.Outcome = render.Answered
	case store.InteractionTimedOut:
		e.Outcome = render.TimedOut
	case store.InteractionCancelled:
		e.Outcome = render.Cancelled
	default:
		return slack.Content{}, fmt.Errorf("an interaction that is %q has no replacement", row.State)
	}
	var in dialect.Interaction
	if err := json.Unmarshal(row.Request, &in); err != nil {
		return slack.Content{}, fmt.Errorf("the stored request: %w", err)
	}
	e.Hint, e.Questions = in.Hint, in.Questions
	if in.Nested != nil {
		e.Subagent = in.Nested.Subagent
	}
	if e.Outcome == render.Answered {
		var a dialect.Answer
		if err := json.Unmarshal(row.Answer, &a); err != nil {
			return slack.Content{}, fmt.Errorf("the stored answer: %w", err)
		}
		e.Answers = a.Answers
	}
	text, blocks := render.Replacement(e)
	raw, err := json.Marshal(blocks)
	if err != nil {
		return slack.Content{}, err
	}
	return slack.Content{Text: text, Blocks: raw}, nil
}
