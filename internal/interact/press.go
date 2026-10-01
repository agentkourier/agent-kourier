package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// PressAuthzTimeout is the bound that the Slack client's membership and bot checks get when a press is
// processed: they run on a worker, off Slack's 3 second acknowledgement path. The composition sets
// slack.Config.AuthzTimeout to it, and a non-positive value there means the client's own default.
const PressAuthzTimeout = 10 * time.Second

// errNotDecidable is what Process returns for a press that is found and open: WP-33d1c2 adds who may
// answer, reading the answer and deciding it.
var errNotDecidable = errors.New("interact: an open press cannot be decided yet")

// ErrNotInteraction is returned by Press for a button whose value is not an interaction's ID: it is
// someone else's button, for the router to hand to another handler.
var ErrNotInteraction = errors.New("interact: the pressed button is not an interaction's")

// The fixed texts told to a person alone. None holds anything the agent wrote.
const (
	textGone     = "This question is no longer available."
	textNotReady = "This question is not ready yet. Please press Submit again in a moment."
)

// PressStore is what the pipeline keeps and reads: the presses waiting, and the interactions they answer.
type PressStore interface {
	store.Presses
	GetInteraction(ctx context.Context, id string) (store.Interaction, error)
}

// PressConfig configures a Pipeline. Store and Ephemerals are required.
type PressConfig struct {
	Store      PressStore
	Ephemerals Ephemeral
	Now        func() time.Time // time.Now if nil
	Logger     *slog.Logger     // slog.Default() if nil
	// Limits bounds the presses waiting (default 200 in all, 10 for one person).
	Limits store.PressLimits
	// EarlyFor is how long a press for a prompt whose message is not recorded yet is tried again
	// (default 2 minutes), before the person is told to press again.
	EarlyFor time.Duration
	// Notify, if set, is called with each press recorded, for the worker (WP-33d1d) to pick it up.
	Notify func(store.Press)
}

// Pipeline takes the presses of a prompt's Submit button (spec 8.4 step 3). Press is the sink's side:
// it records the press and returns, so Slack's acknowledgement is not kept waiting. Process is the
// worker's: it finds the interaction, checks who pressed, reads the answer and hands the complete one to
// the decider. Slack never sees what Process finds, so every outcome that is not a decision reaches the
// person as an ephemeral message, and none ever updates the prompt (an update would reset every other
// person's unsubmitted inputs).
type Pipeline struct{ cfg PressConfig }

// NewPipeline returns a Pipeline over cfg.
func NewPipeline(cfg PressConfig) (*Pipeline, error) {
	if cfg.Store == nil || cfg.Ephemerals == nil {
		return nil, errors.New("interact: Store and Ephemerals are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Limits.Total <= 0 || cfg.Limits.PerUser <= 0 {
		cfg.Limits = store.PressLimits{Total: 200, PerUser: 10}
	}
	if cfg.EarlyFor <= 0 {
		cfg.EarlyFor = 2 * time.Minute
	}
	return &Pipeline{cfg}, nil
}

// Press records a press of an interaction's button and returns. PressedAt is courier's own clock now,
// not Slack's, and is the time of the decision whenever it is made (WP-33e contract). It returns
// ErrNotInteraction for a button that is not an interaction's, and store.ErrLimit when too many presses
// wait.
func (p *Pipeline) Press(ctx context.Context, i slack.Interaction) error {
	if !strings.HasPrefix(i.Value, "ix-") {
		return ErrNotInteraction
	}
	inputs, err := json.Marshal(i.Inputs)
	if err != nil {
		return fmt.Errorf("interact: encode a press: %w", err)
	}
	press := store.Press{InteractionID: i.Value, User: i.User, Channel: i.Channel, MessageTS: i.MessageTS, Inputs: inputs, PressedAt: p.cfg.Now()}
	if press.Seq, err = p.cfg.Store.RecordPress(ctx, press, p.cfg.Limits); err != nil {
		return fmt.Errorf("interact: record a press of %s: %w", i.Value, err)
	}
	if p.cfg.Notify != nil {
		p.cfg.Notify(press)
	}
	return nil
}

// Result is what Process did with a press.
type Result int

// The results.
const (
	// Done: the press is processed, and deleted.
	Done Result = iota
	// Retry: the press cannot be processed yet, and is kept for another try.
	Retry
)

// Process works one recorded press through: find the interaction (a press for none, or for another
// message than the prompt's, is dropped), and see that it is still open (anyone else's press on an
// answered or closed one is told who answered, or that it is closed). A press for a prompt whose message
// is not recorded yet is tried again for EarlyFor. The press is deleted only when it is finished.
func (p *Pipeline) Process(ctx context.Context, press store.Press) (Result, error) {
	row, err := p.cfg.Store.GetInteraction(ctx, press.InteractionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return p.finish(ctx, press, "", textGone)
	case err != nil:
		return Retry, fmt.Errorf("interact: read %s: %w", press.InteractionID, err)
	case row.MessageRef == "":
		if p.cfg.Now().Sub(press.PressedAt) < p.cfg.EarlyFor { // the prompt's post is not recorded yet
			return Retry, nil
		}
		return p.finish(ctx, press, row.Key.ThreadTS, textNotReady)
	case row.MessageRef != press.MessageTS || row.Key.Channel != press.Channel:
		p.cfg.Logger.WarnContext(ctx, "interact: a press names an interaction that is on another message", "interaction", row.ID)
		return p.finish(ctx, press, "", "")
	case !p.open(row, press.User):
		return p.finish(ctx, press, row.Key.ThreadTS, lostText(row))
	}
	return Retry, errNotDecidable
}

// open reports whether user's press can still be decided: the question is pending, or user's own answer
// is recorded and the press is the same person's retry after a crash, which the decider sends again.
func (p *Pipeline) open(row store.Interaction, user string) bool {
	return row.State == store.InteractionPending || (row.State == store.InteractionAnswered && row.Decider == user)
}

// finish tells the person text, if there is any, in thread (the interaction's: a prompt lives in a thread), and deletes the press. Telling is best-effort: a person
// who is not told can press again, and a failure is only logged.
func (p *Pipeline) finish(ctx context.Context, press store.Press, thread, text string) (Result, error) {
	if text != "" {
		err := p.cfg.Ephemerals.PostEphemeral(ctx, press.Channel, thread, press.User, slack.Content{Text: text})
		if err != nil {
			p.cfg.Logger.WarnContext(ctx, "interact: a person could not be told the outcome of their press", "interaction", press.InteractionID, "err", err)
		}
	}
	if err := p.cfg.Store.DeletePress(ctx, press.Seq); err != nil {
		return Retry, fmt.Errorf("interact: delete the press %d: %w", press.Seq, err)
	}
	return Done, nil
}
