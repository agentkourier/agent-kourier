package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// The fixed texts told to a person alone. None holds anything the agent wrote.
const (
	textLate      = "This question has timed out, so your answer was not taken."
	textInvalid   = "That answer could not be used. Please check it and press Submit again."
	textTimedOut  = "This question has timed out."
	textClosed    = "This question is closed."
	textFirstNone = "Someone else answered this question first."
)

// slackUserID is the shape of a Slack user ID, which only is named to a person in a mention.
var slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// Ephemeral shows a message to one person alone: *slack.Client.
type Ephemeral interface {
	PostEphemeral(ctx context.Context, channel, threadTS, user string, content slack.Content) error
}

// PressDecision is a press whose answer is complete: the pipeline (WP-33d1c) has found its interaction,
// checked who pressed it and read its inputs, and hands over what is left to decide.
type PressDecision struct {
	// Interaction is the row the press was found for; its state is read again when it is decided.
	Interaction store.Interaction
	// In is the decoded request of the interaction, and Answer the person's complete answer to it.
	In     dialect.Interaction
	Answer dialect.Answer
	// User is who pressed. PressedAt is when courier recorded the press, by its own clock, and is the
	// time of the decision whenever it is decided (WP-33e contract): a press made in time wins even if
	// it is decided after the deadline.
	User      string
	PressedAt time.Time
	// BindingName names the Binding of the thread for the audit row, and Binding is what it allows.
	BindingName string
	Binding     Binding
}

// DecideConfig configures a Decider. Store, Auditor and Ephemerals are required.
type DecideConfig struct {
	Store      store.Interactions
	Auditor    Auditor
	Ephemerals Ephemeral
	Logger     *slog.Logger // slog.Default() if nil
	// OnDecision, if set, is called once for each press decided, with its result: "answered" (it
	// won), "superseded" (another decision came first), "late" (made at or after the deadline) or
	// "invalid" (the dialect refused the answer). It is for a metric.
	OnDecision func(result string)
}

// Decider takes the decision on a press: first answer wins (spec 8.4 step 5). It is safe for
// concurrent use, and takes one decision at a time, so that of two presses the second reads what the
// first did.
type Decider struct {
	cfg DecideConfig
	mu  sync.Mutex
}

// NewDecider returns a Decider over cfg.
func NewDecider(cfg DecideConfig) (*Decider, error) {
	if cfg.Store == nil || cfg.Auditor == nil || cfg.Ephemerals == nil {
		return nil, errors.New("interact: Store, Auditor and Ephemerals are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Decider{cfg: cfg}, nil
}

// verdict is how a press came out.
type verdict int

const (
	won   verdict = iota // it is the answer
	again                // the same person already won: this is the press decided again after a crash
	late                 // made at or after the deadline
	lost                 // another decision was first, or the question is closed
	gone                 // there is no such interaction
)

// Decide takes the decision, in this order: build the message that answers, audit the decision, record
// it, send it (WP-33d2b). Building comes first because it is validation and not an action: an answer the
// dialect refuses is told to the person, and nothing is recorded, so no interaction is left answered
// with an answer that can never be sent. The audit comes before the decision takes effect, and a failed
// audit write stops it (spec 8.6 step 7): the error is returned, and the press, which stays recorded, is
// decided again. It returns nil when the press is decided for good, whichever way it came out.
//
// The audit is at least once, and written before the decision takes effect, so it can be ahead of what
// happened. If the sweep, or another process, decides in the window between the read and the write, the
// press has an `answered` row for a decision that never took effect, followed by `superseded`. A crash
// between the audit and the write audits `answered` again when the press is decided again. Both are
// accepted over a decision that takes effect unaudited.
//
// The interaction is recorded as answered at PressedAt, so a press made before the deadline wins even if
// it is decided after it. One made at or after the deadline is no decision: it is told so and not audited.
func (d *Decider) Decide(ctx context.Context, pd PressDecision) error {
	row := pd.Interaction
	msg, err := AnswerMessage(pd.Binding.Dialect, row.ID, pd.In, pd.Answer, pd.User)
	if err != nil {
		d.cfg.Logger.InfoContext(ctx, "interact: the dialect refused an answer", "interaction", row.ID, "err", err)
		d.tell(ctx, row, pd.User, textInvalid)
		d.count("invalid")
		return nil
	}
	d.mu.Lock()
	v, cur, err := d.take(ctx, pd)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	switch v {
	case won:
		d.count("answered")
	case late:
		d.tell(ctx, row, pd.User, textLate)
		d.count("late")
		return nil
	case lost:
		d.tell(ctx, row, pd.User, lostText(cur))
		d.count("superseded")
		return nil
	case gone:
		return nil
	}
	return d.send(ctx, pd, msg)
}

// take reads the interaction and records the decision if the press can win. The caller holds d.mu.
func (d *Decider) take(ctx context.Context, pd PressDecision) (verdict, store.Interaction, error) {
	cur, err := d.cfg.Store.GetInteraction(ctx, pd.Interaction.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return gone, cur, nil
	case err != nil:
		return 0, cur, fmt.Errorf("interact: read %s: %w", pd.Interaction.ID, err)
	}
	if cur.State != store.InteractionPending {
		return d.settle(ctx, pd, cur)
	}
	if !pd.PressedAt.Before(cur.Deadline) {
		return late, cur, nil
	}
	actor := audit.Actor{User: pd.User, Binding: pd.BindingName, Identity: pd.Binding.Identity}
	decision := audit.Decision{InteractionID: cur.ID, TaskID: a2a.TaskID(cur.TaskID), Result: audit.DecisionAnswered, At: pd.PressedAt, Answer: &pd.Answer}
	if err := d.cfg.Auditor.Decision(ctx, actor, decision); err != nil {
		return 0, cur, fmt.Errorf("interact: audit the decision on %s: %w", cur.ID, err)
	}
	answer, err := json.Marshal(pd.Answer)
	if err != nil {
		return 0, cur, fmt.Errorf("interact: encode the answer to %s: %w", cur.ID, err)
	}
	switch err := d.cfg.Store.SubmitInteraction(ctx, cur.ID, answer, pd.User, pd.PressedAt); {
	case err == nil:
		return won, cur, nil
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrExpired), errors.Is(err, store.ErrNotFound):
		// Decided between the read and the write, by another process or the sweep: see how.
		if cur, err = d.cfg.Store.GetInteraction(ctx, cur.ID); err != nil {
			return gone, cur, nil
		}
		if cur.State == store.InteractionPending {
			return late, cur, nil
		}
		return d.settle(ctx, pd, cur)
	default:
		return 0, cur, fmt.Errorf("interact: record the decision on %s: %w", cur.ID, err)
	}
}

// settle is the verdict on a press for an interaction that is no longer pending: the press decided
// again by the person who won is a retry; one made at or after the deadline is late, as it is for a
// pending one, and is not audited; anyone else lost, which is audited. The caller holds d.mu.
func (d *Decider) settle(ctx context.Context, pd PressDecision, cur store.Interaction) (verdict, store.Interaction, error) {
	if cur.State == store.InteractionAnswered && cur.Decider == pd.User {
		return again, cur, nil
	}
	if !pd.PressedAt.Before(cur.Deadline) {
		return late, cur, nil
	}
	actor := audit.Actor{User: pd.User, Binding: pd.BindingName, Identity: pd.Binding.Identity}
	decision := audit.Decision{InteractionID: cur.ID, TaskID: a2a.TaskID(cur.TaskID), Result: audit.DecisionSuperseded, At: pd.PressedAt}
	if err := d.cfg.Auditor.Decision(ctx, actor, decision); err != nil { // the loss is done, and told: log it
		d.cfg.Logger.ErrorContext(ctx, "interact: a lost press was not audited", "interaction", cur.ID, "err", err)
	}
	return lost, cur, nil
}

// send is where the answer reaches the agent (WP-33d2b).
func (d *Decider) send(context.Context, PressDecision, a2a.Message) error { return nil }

// lostText says why a press did not decide the question.
func lostText(cur store.Interaction) string {
	switch {
	case cur.State == store.InteractionTimedOut:
		return textTimedOut
	case cur.State != store.InteractionAnswered:
		return textClosed
	case slackUserID.MatchString(cur.Decider):
		return "<@" + cur.Decider + "> answered this question first."
	}
	return textFirstNone
}

// tell shows text to the person alone. It is told once and kept nowhere, so a failure is only logged.
func (d *Decider) tell(ctx context.Context, row store.Interaction, user, text string) {
	if err := d.cfg.Ephemerals.PostEphemeral(ctx, row.Key.Channel, row.Key.ThreadTS, user, slack.Content{Text: text}); err != nil {
		d.cfg.Logger.WarnContext(ctx, "interact: a person could not be told the outcome of their press", "interaction", row.ID, "err", err)
	}
}

func (d *Decider) count(result string) {
	if d.cfg.OnDecision != nil {
		d.cfg.OnDecision(result)
	}
}
