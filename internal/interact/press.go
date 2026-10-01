package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// PressAuthzTimeout is the bound that the Slack client's membership and bot checks get when a press is
// processed: they run on a worker, off Slack's 3 second acknowledgement path. The composition sets
// slack.Config.AuthzTimeout to it, and a non-positive value there means the client's own default.
const PressAuthzTimeout = 10 * time.Second

// ErrNotInteraction is returned by Press for a button whose value is not an interaction's ID: it is
// someone else's button, for the router to hand to another handler.
var ErrNotInteraction = errors.New("interact: the pressed button is not an interaction's")

// The fixed texts told to a person alone. None holds anything the agent wrote.
const (
	textGone       = "This question is no longer available."
	textNotReady   = "This question is not ready yet. Please press Submit again in a moment."
	textNotMember  = "Only members of this channel can answer this question."
	textUnverified = "Couldn't verify that you can answer this question. Please try again."
)

// Authorizer says who may answer: *slack.Client. Both fail closed, IsBot by returning true with its error.
type Authorizer interface {
	IsChannelMember(ctx context.Context, channel, user string) (bool, error)
	IsBot(ctx context.Context, user string) (bool, error)
}

// PressDecider takes the decision on a complete press: *Decider.
type PressDecider interface {
	Decide(ctx context.Context, pd PressDecision) error
}

// PressStore is what the pipeline keeps and reads: the presses waiting, and the interactions they answer.
type PressStore interface {
	store.Presses
	GetInteraction(ctx context.Context, id string) (store.Interaction, error)
}

// PressConfig configures a Pipeline. Everything but Now, Logger, Limits, EarlyFor and Notify is required.
type PressConfig struct {
	Store      PressStore
	Sessions   SessionReader
	Lookup     Lookup
	Authorizer Authorizer
	Ephemerals Ephemeral
	Decider    PressDecider
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
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Lookup == nil || cfg.Authorizer == nil || cfg.Ephemerals == nil || cfg.Decider == nil {
		return nil, errors.New("interact: Store, Sessions, Lookup, Authorizer, Ephemerals and Decider are required")
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

// Process works one recorded press through, in this order: find the interaction (a press for none, or
// for another message than the prompt's, is dropped), see that it is still open (anyone else's press on
// an answered or closed one is told who answered, or that it is closed), check that the person is a
// member and not a bot, read their answer, and hand a complete one to the decider. The press is deleted
// only when it is finished: after the decider returns nil, or after a refusal that is final. A failure
// to check, or to decide, is an error and leaves the press for another try.
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
	return p.authorized(ctx, press, row)
}

// open reports whether user's press can still be decided: the question is pending, or user's own answer
// is recorded and the press is the same person's retry after a crash, which the decider sends again.
func (p *Pipeline) open(row store.Interaction, user string) bool {
	return row.State == store.InteractionPending || (row.State == store.InteractionAnswered && row.Decider == user)
}

// authorized checks who pressed, reads the answer and decides it.
func (p *Pipeline) authorized(ctx context.Context, press store.Press, row store.Interaction) (Result, error) {
	switch member, err := p.cfg.Authorizer.IsChannelMember(ctx, row.Key.Channel, press.User); {
	case err != nil:
		return p.finish(ctx, press, row.Key.ThreadTS, textUnverified)
	case !member:
		return p.finish(ctx, press, row.Key.ThreadTS, textNotMember)
	}
	if bot, err := p.cfg.Authorizer.IsBot(ctx, press.User); err != nil {
		return p.finish(ctx, press, row.Key.ThreadTS, textUnverified)
	} else if bot {
		p.cfg.Logger.InfoContext(ctx, "interact: a bot pressed a prompt's button", "interaction", row.ID)
		return p.finish(ctx, press, "", "")
	}
	var in dialect.Interaction
	var inputs map[string]map[string]slack.Input
	if err := errors.Join(json.Unmarshal(row.Request, &in), json.Unmarshal(press.Inputs, &inputs)); err != nil {
		p.cfg.Logger.ErrorContext(ctx, "interact: a press or its interaction cannot be read", "interaction", row.ID, "err", err)
		return p.finish(ctx, press, row.Key.ThreadTS, textGone)
	}
	controls, err := Controls(row.ID, in)
	if err != nil {
		return p.finish(ctx, press, row.Key.ThreadTS, textGone)
	}
	answer, problems, err := ExtractAnswer(in, controls, inputs)
	if err != nil {
		return p.finish(ctx, press, row.Key.ThreadTS, textGone)
	} else if len(problems) > 0 {
		return p.finish(ctx, press, row.Key.ThreadTS, incompleteText(problems))
	}
	name, b, err := p.binding(ctx, row)
	if err != nil {
		return Retry, err
	} else if b == nil {
		return p.finish(ctx, press, row.Key.ThreadTS, textGone)
	}
	pd := PressDecision{Interaction: row, In: in, Answer: answer, User: press.User, PressedAt: press.PressedAt, BindingName: name, Binding: *b}
	if err := p.cfg.Decider.Decide(ctx, pd); err != nil {
		return Retry, err
	}
	return p.finish(ctx, press, "", "")
}

// binding is the Binding of the interaction's thread, or nil if the thread or its Binding is gone.
func (p *Pipeline) binding(ctx context.Context, row store.Interaction) (string, *Binding, error) {
	s, err := p.cfg.Sessions.GetSession(ctx, row.Key, readAll, p.cfg.Now())
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, fmt.Errorf("interact: read the session of %s: %w", row.ID, err)
	}
	b, err := p.cfg.Lookup.Binding(s.Binding)
	if err != nil { // a Binding removed from the config: the press can never be decided
		p.cfg.Logger.WarnContext(ctx, "interact: the Binding of a press is gone", "binding", s.Binding, "err", err)
		return "", nil, nil
	}
	return s.Binding, &b, nil
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

// incompleteText names the questions that need an answer, by number only.
func incompleteText(problems []Problem) string {
	var unanswered, invalid []string
	for _, pr := range problems {
		if pr.Kind == Invalid {
			invalid = append(invalid, strconv.Itoa(pr.Question+1))
		} else {
			unanswered = append(unanswered, strconv.Itoa(pr.Question+1))
		}
	}
	switch {
	case len(invalid) > 0:
		return "Question " + strings.Join(invalid, ", ") + " could not be read. Please check it and press Submit again."
	default:
		return "Please answer question " + strings.Join(unanswered, ", ") + ", then press Submit again."
	}
}
