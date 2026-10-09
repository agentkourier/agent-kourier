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

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
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
	textGone      = "This question is no longer available."
	textNotReady  = "This question is not ready yet. Please press Submit again in a moment."
	textNotMember = "Only members of this channel can answer this question."
)

// Authorizer says who may answer: a chat.Platform. Both fail closed, IsBot by returning true with its error.
type Authorizer interface {
	IsMember(ctx context.Context, channel, user string) (bool, error)
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
	Auditor    Auditor
	Now        func() time.Time // time.Now if nil
	Logger     *slog.Logger     // slog.Default() if nil
	// Limits bounds the presses waiting (default 1000 in all, 10 for one person).
	Limits store.PressLimits
	// EarlyFor is how long a press for a prompt whose message is not recorded yet is tried again
	// (default 2 minutes), before the person is told to press again. It is for that alone: a press whose
	// check of the person fails is kept until the worker gives up on it.
	EarlyFor time.Duration
	// GiveUpAfter bounds failed text-answer retries; default matches the press worker.
	GiveUpAfter time.Duration
	// Notify is called with each press recorded, for the worker (WP-33d1d) to pick it up. Required. It runs
	// in Press, on the path that acknowledges the button to Slack, so it must never block: a hand-off to a
	// buffered or unbounded queue, as Worker.Submit is, and not to a channel a worker may not be reading.
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
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Lookup == nil || cfg.Authorizer == nil || cfg.Ephemerals == nil || cfg.Decider == nil || cfg.Auditor == nil {
		return nil, errors.New("interact: Store, Sessions, Lookup, Authorizer, Ephemerals, Decider and Auditor are required")
	}
	if cfg.Notify == nil {
		return nil, errors.New("interact: Notify is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Limits.Total <= 0 || cfg.Limits.PerUser <= 0 {
		cfg.Limits = store.PressLimits{Total: 1000, PerUser: 10}
	}
	if cfg.EarlyFor <= 0 {
		cfg.EarlyFor = 2 * time.Minute
	}
	if cfg.GiveUpAfter <= 0 {
		cfg.GiveUpAfter = defaultGiveUpAfter
	}
	return &Pipeline{cfg}, nil
}

// Press records a press of an interaction's button and returns. PressedAt is Agent Kourier's own clock now,
// not Slack's, and is the time of the decision whenever it is made (WP-33e contract). It returns
// ErrNotInteraction for a button that is not an interaction's, and store.ErrLimit when too many presses
// wait.
func (p *Pipeline) Press(ctx context.Context, i chat.Interaction) error {
	if !strings.HasPrefix(i.PromptID, "ix-") {
		return ErrNotInteraction
	}
	answers, err := json.Marshal(i.Answers)
	if err != nil {
		return fmt.Errorf("interact: encode a press: %w", err)
	}
	press := store.Press{InteractionID: i.PromptID, User: i.User, Channel: i.At.Channel, MessageRef: i.At.ID, Inputs: answers, PressedAt: p.cfg.Now()}
	press.TraceParent, press.TraceState = telemetry.Capture(ctx)
	if press.Seq, err = p.cfg.Store.RecordPress(ctx, press, p.cfg.Limits); err != nil {
		return fmt.Errorf("interact: record a press of %s: %w", i.PromptID, err)
	}
	p.cfg.Notify(press)
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
// only when it is finished: after the decider returns nil, or after a refusal that is final. A person
// refused as not a member, or a bot, or for an incomplete answer, is audited first (denied, or incomplete
// with the answer's shape and no values); a press on a question that is already decided is not, since only
// a press that races into the decider is a decision lost. A failure to check, to audit or to decide is an
// error and leaves the press for another try, however old it is: a check that cannot be made is no decision,
// and only the worker's cap ends such a press (GiveUp).
func (p *Pipeline) Process(ctx context.Context, press store.Press) (Result, error) {
	row, err := p.cfg.Store.GetInteraction(ctx, press.InteractionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return p.finish(ctx, press, nil, textGone)
	case err != nil:
		return Retry, fmt.Errorf("interact: read %s: %w", press.InteractionID, err)
	case row.MessageRef == "":
		if p.cfg.Now().Sub(press.PressedAt) < p.cfg.EarlyFor { // the prompt's post is not recorded yet
			return Retry, nil
		}
		return p.finish(ctx, press, &row, textNotReady)
	case row.MessageRef != press.MessageRef || row.Key.Channel != press.Channel:
		p.cfg.Logger.WarnContext(ctx, "interact: a press names an interaction that is on another message", "interaction", row.ID)
		return p.finish(ctx, press, nil, "")
	case !p.open(row, press.User):
		return p.finishWith(ctx, press, &row, lostMessage(row))
	}
	return p.authorized(ctx, press, row)
}

// open reports whether user's press can still be decided: the question is pending, or user's own answer
// is recorded and the press is the same person's retry after a crash, which the decider sends again.
func (p *Pipeline) open(row store.Interaction, user string) bool {
	return row.State == store.InteractionPending || (row.State == store.InteractionAnswered && row.Decider == user)
}

// authorized checks who pressed, reads the answer and decides it. A person refused as not a member, or a
// bot, or whose answer is incomplete or unreadable, is a decision of its own and is audited, before the
// person is told, and a failed audit write keeps the press for another try (spec 8.6 step 7). A check that
// itself fails is no decision, and is only told.
func (p *Pipeline) authorized(ctx context.Context, press store.Press, row store.Interaction) (Result, error) {
	name, b, err := p.binding(ctx, row)
	if err != nil {
		return Retry, err
	} else if b == nil {
		return p.finish(ctx, press, &row, textGone)
	}
	refuse := func(result audit.DecisionResult, answered *dialect.Answer, text string) (Result, error) {
		actor := audit.Actor{User: press.User, Binding: name, Identity: b.Identity}
		d := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: result, At: press.PressedAt, Answer: answered}
		if err := p.cfg.Auditor.Decision(ctx, actor, d); err != nil {
			return Retry, fmt.Errorf("interact: audit the refusal of a press of %s: %w", row.ID, err)
		}
		return p.finish(ctx, press, &row, text)
	}
	switch member, err := p.cfg.Authorizer.IsMember(ctx, row.Key.Channel, press.User); {
	case err != nil:
		return p.unverified(row, err)
	case !member:
		return refuse(audit.DecisionDenied, nil, textNotMember)
	}
	if bot, err := p.cfg.Authorizer.IsBot(ctx, press.User); err != nil {
		return p.unverified(row, err)
	} else if bot {
		return refuse(audit.DecisionDenied, nil, "")
	}
	if len(row.Request) == 0 { // cleared by retention once the interaction was settled long enough: no question to answer
		return p.finish(ctx, press, &row, textGone)
	}
	var in dialect.Interaction
	if err := json.Unmarshal(row.Request, &in); err != nil {
		p.cfg.Logger.ErrorContext(ctx, "interact: the request of an interaction cannot be read", "interaction", row.ID, "err", err)
		return p.finish(ctx, press, &row, textGone)
	}
	var answers []chat.Answer
	if err := json.Unmarshal(press.Inputs, &answers); err != nil {
		// What a press recorded before the answers were neutral holds: Slack's state by block and action. It is
		// no corruption, and it passes: the person is told the question is gone.
		p.cfg.Logger.WarnContext(ctx, "interact: a press recorded by an older version cannot be read; the person is told the question is gone", "interaction", row.ID, "err", err)
		return p.finish(ctx, press, &row, textGone)
	}
	answer, problems, err := ExtractAnswer(in, answers)
	if err != nil {
		return p.finish(ctx, press, &row, textGone)
	} else if len(problems) > 0 {
		shape := &dialect.Answer{Answers: make([][]string, len(in.Questions)-len(problems))} // how many were answered, never what
		return refuse(audit.DecisionIncomplete, shape, incompleteText(problems))
	}
	pd := PressDecision{Interaction: row, In: in, Answer: answer, User: press.User, PressedAt: press.PressedAt, BindingName: name, Binding: *b}
	if err := p.cfg.Decider.Decide(ctx, pd); err != nil {
		return Retry, err
	}
	return p.finish(ctx, press, nil, "")
}

// unverified is a check of who pressed that could not be made. It is no decision, so the press is kept for
// the worker to try again, at whatever age: a Slack error that outlasts a window, and straddles the deadline,
// must not lose an answer that came in time. Only the worker's cap ends such a press, through GiveUp, which
// tells the person to press again.
func (p *Pipeline) unverified(row store.Interaction, err error) (Result, error) {
	return Retry, fmt.Errorf("interact: check who pressed %s: %w", row.ID, err)
}

// textGaveUp is told to a person whose press the worker gave up on. Pressing again is the way on: an
// answer already recorded is sent again, and one that is not is decided.
const textGaveUp = "Couldn't process your answer. Please press Submit again."

// GiveUp ends a press that could not be processed in time: the person is told to press again, and the
// press is deleted, so that the timeout sweep, which leaves an interaction with a press waiting, can go on
// with it. Telling is best-effort. An error means the press is still recorded.
func (p *Pipeline) GiveUp(ctx context.Context, press store.Press) error {
	var in *store.Interaction
	if row, err := p.cfg.Store.GetInteraction(ctx, press.InteractionID); err == nil {
		in = &row
	}
	_, err := p.finish(ctx, press, in, textGaveUp)
	return err
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

// finish tells the person text, if there is any, in the thread of row (the interaction's: a prompt lives in a
// thread; nil when it is not known), and deletes the press. Telling is best-effort: a person who is not told
// can press again, and a failure is only logged.
func (p *Pipeline) finish(ctx context.Context, press store.Press, row *store.Interaction, text string) (Result, error) {
	return p.finishWith(ctx, press, row, chat.Message{Fallback: text})
}

// finishWith is finish for a message that has parts, such as one that names a person.
func (p *Pipeline) finishWith(ctx context.Context, press store.Press, row *store.Interaction, m chat.Message) (Result, error) {
	if m.Fallback != "" || len(m.Parts) > 0 {
		to := chat.Target{Channel: press.Channel}
		if row != nil {
			to.Conn, to.Thread = row.Key.Connection, row.Key.Thread
		}
		if err := p.cfg.Ephemerals.Ephemeral(ctx, to, press.User, m); err != nil {
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
