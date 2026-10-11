package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// The fixed texts told to a person alone about a question. None holds anything the agent wrote. An approval has its
// own wording (words.go), chosen by the kind of the request.
const (
	textGone      = "This question is no longer available."
	textNotReady  = "This question is not ready yet. Please press Submit again in a moment."
	textNotMember = "Only members of this channel can answer this question."
)

// Authorizer says who may answer: a chat.Platform. All fail closed: IsBot by returning true with its error, and IsInAnyGroup
// by returning false with its error, so an error is never a yes. IsInAnyGroup is asked only for a tool approval on a Binding
// that names approver groups.
type Authorizer interface {
	IsMember(ctx context.Context, channel, user string) (bool, error)
	IsBot(ctx context.Context, user string) (bool, error)
	IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error)
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
	inputs, err := encodePress(i)
	if err != nil {
		return fmt.Errorf("interact: encode a press: %w", err)
	}
	press := store.Press{InteractionID: i.PromptID, User: i.User, Channel: i.At.Channel, MessageRef: i.At.ID, Inputs: inputs, PressedAt: p.cfg.Now()}
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
// member and not a bot (for an approval on a Binding with approver groups, also in one of them, unless their own answer is
// already recorded), read their answer (a question's answers, or an approval's button and per-tool verdicts), and hand a
// complete one to the decider. The press is deleted
// only when it is finished: after the decider returns nil, or after a refusal that is final. A person
// refused as not a member, or a bot (denied), or in none of the approver groups (denied_not_approver), or for an incomplete
// answer, is audited first (incomplete
// with the answer's shape and no values: how many were answered, or for an approval how many tools were decided); a press on a question that is already decided is not, since only
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
		return p.finish(ctx, press, &row, wordsOf(requestKind(row)).notReady)
	case row.MessageRef != press.MessageRef || row.Key.Channel != press.Channel:
		p.cfg.Logger.WarnContext(ctx, "interact: a press names an interaction that is on another message", "interaction", row.ID)
		return p.finish(ctx, press, nil, "")
	case !p.open(row, press.User):
		return p.finishWith(ctx, press, &row, lostMessage(wordsOf(requestKind(row)), row))
	}
	return p.authorized(ctx, press, row)
}

// open reports whether user's press can still be decided: the question is pending, or user's own answer
// is recorded and the press is the same person's retry after a crash, which the decider sends again. That retry decides
// nothing, so it is not asked about the approver groups.
func (p *Pipeline) open(row store.Interaction, user string) bool {
	return row.State == store.InteractionPending || (row.State == store.InteractionAnswered && row.Decider == user)
}

// authorized checks who pressed, reads the answer and decides it. A person refused as not a member, or a bot, or in none of
// the approver groups, or whose answer is incomplete or unreadable, is a decision of its own and is audited, before the
// person is told, and a failed audit write keeps the press for another try (spec 8.6 step 7). A check that
// itself fails is no decision, and is only told.
//
// What the press means depends on the kind of the decoded request, never on the row's kind (a tool approval that a
// sub-agent raised is stored as "nested"). A question reads the controls' answers (ExtractAnswer); an approval reads
// the button and the per-tool choices (ExtractDecisions). Either goes on to the same decider.
func (p *Pipeline) authorized(ctx context.Context, press store.Press, row store.Interaction) (Result, error) {
	name, b, err := p.binding(ctx, row)
	if err != nil {
		return Retry, err
	} else if b == nil {
		return p.finish(ctx, press, &row, wordsOf(requestKind(row)).gone)
	}
	w := wordsOf(requestKind(row))
	refuse := func(d audit.Decision, text string) (Result, error) {
		actor := audit.Actor{User: press.User, Binding: name, Identity: b.Identity}
		d.InteractionID, d.TaskID, d.At = row.ID, a2a.TaskID(row.TaskID), press.PressedAt
		if err := p.cfg.Auditor.Decision(ctx, actor, d); err != nil {
			return Retry, fmt.Errorf("interact: audit the refusal of a press of %s: %w", row.ID, err)
		}
		return p.finish(ctx, press, &row, text)
	}
	switch member, err := p.cfg.Authorizer.IsMember(ctx, row.Key.Channel, press.User); {
	case err != nil:
		return p.unverified(row, err)
	case !member:
		return refuse(audit.Decision{Result: audit.DecisionDenied}, w.notMember)
	}
	if bot, err := p.cfg.Authorizer.IsBot(ctx, press.User); err != nil {
		return p.unverified(row, err)
	} else if bot {
		return refuse(audit.Decision{Result: audit.DecisionDenied}, "")
	}
	if len(row.Request) == 0 { // cleared by retention once the interaction was settled long enough: no question to answer
		return p.finish(ctx, press, &row, w.gone)
	}
	var in dialect.Interaction
	if err := json.Unmarshal(row.Request, &in); err != nil {
		p.cfg.Logger.ErrorContext(ctx, "interact: the request of an interaction cannot be read", "interaction", row.ID, "err", err)
		return p.finish(ctx, press, &row, w.gone)
	}
	inputs, err := decodePress(press.Inputs)
	if err != nil {
		// What a press recorded before the answers were neutral holds (Slack's state by block and action), or what another
		// version recorded in a form this one does not know, such as an action it lacks. It is no corruption, and it
		// passes: the person is told the question is gone.
		p.cfg.Logger.WarnContext(ctx, "interact: a press recorded in a form this version does not read (an older version's, or another's); the person is told the request is gone", "interaction", row.ID, "err", err)
		return p.finish(ctx, press, &row, w.gone)
	}
	var (
		answer   dialect.Answer
		problems []Problem
		shape    audit.Decision
		text     func([]Problem) string
	)
	if in.Kind == dialect.ToolApproval {
		// Switched off since the prompt was posted: nothing is decided from this press. What the pause does when it comes again
		// depends on the row (autoDeny): a pending one is denied by policy, and for one that a person already decided the thread is
		// told that the request is being denied (not true of that decision) and the recorded answer is sent again.
		if !b.ToolApprovals {
			p.cfg.Logger.WarnContext(ctx, "interact: a press on an approval whose Binding no longer allows approvals; nothing is decided", "interaction", row.ID)
			return p.finish(ctx, press, &row, w.gone)
		}
		// The winner's own retry (open) is not asked: their approval was authorized when it was made, and the retry only sends
		// the recorded answer again; refusing it would leave a decision audited and not delivered, and put a refusal after it.
		if len(b.ApproverGroups) > 0 && (row.State != store.InteractionAnswered || row.Decider != press.User) {
			switch approver, err := p.cfg.Authorizer.IsInAnyGroup(ctx, b.ApproverGroups, press.User); {
			case err != nil:
				// Usually a fault in the configuration (a group that is mistyped or disabled, a scope the app was installed without)
				// that no retry mends, so it is logged where an operator will see it. It is no denial of the person: never audited as one.
				// A cancelled context is a shutdown with the press in flight, not a fault of the groups, and is not logged.
				if ctx.Err() == nil {
					p.cfg.Logger.ErrorContext(ctx, "interact: the approver-group check failed; the press is kept and tried again", "binding", name, "interaction", row.ID, "err", err)
				}
				return Retry, fmt.Errorf("interact: check the approver groups of %s for %s: %w", name, row.ID, err)
			case !approver:
				return refuse(audit.Decision{Result: audit.DecisionNotApprover}, w.notApprover)
			}
		}
		answer, problems, err = ExtractDecisions(in, inputs.Action, inputs.Answers)
		shape = audit.Decision{Result: audit.DecisionIncomplete, Shape: &audit.ToolShape{Decided: len(in.Tools) - len(problems), Total: len(in.Tools)}}
		text = incompleteToolsText
	} else {
		answer, problems, err = askUserAnswer(in, inputs)
		shape = audit.Decision{Result: audit.DecisionIncomplete, Answer: &dialect.Answer{Answers: make([][]string, len(in.Questions)-len(problems))}} // how many were answered, never what
		text = incompleteText
	}
	if err != nil {
		return p.finish(ctx, press, &row, w.gone)
	} else if len(problems) > 0 {
		return refuse(shape, text(problems))
	}
	pd := PressDecision{Interaction: row, In: in, Answer: answer, User: press.User, PressedAt: press.PressedAt, BindingName: name, Binding: *b}
	if err := p.cfg.Decider.Decide(ctx, pd); err != nil {
		return Retry, err
	}
	return p.finish(ctx, press, nil, "")
}

// askUserAnswer reads a press of a question. Submit is its one button: a press of any other is no honest press of the
// controls the adapter made from this prompt, and every question of it is refused as invalid, as a stray answer is.
func askUserAnswer(in dialect.Interaction, inputs pressInputs) (dialect.Answer, []Problem, error) {
	if inputs.Action != chat.ActionSubmit && in.Kind == dialect.AskUser {
		problems := make([]Problem, len(in.Questions))
		for i := range problems {
			problems[i] = Problem{Question: i, Kind: Invalid}
		}
		return dialect.Answer{}, problems, nil
	}
	return ExtractAnswer(in, inputs.Answers)
}

// unverified is a check of who pressed that could not be made. It is no decision, so the press is kept for
// the worker to try again, at whatever age: a Slack error that outlasts a window, and straddles the deadline,
// must not lose an answer that came in time. Only the worker's cap ends such a press, through GiveUp, which
// tells the person to press again.
func (p *Pipeline) unverified(row store.Interaction, err error) (Result, error) {
	return Retry, fmt.Errorf("interact: check who pressed %s: %w", row.ID, err)
}

// textGaveUp is told to a person whose press the worker gave up on, about a question. Pressing again is the way on: an
// answer already recorded is sent again, and one that is not is decided.
const textGaveUp = "Couldn't process your answer. Please press Submit again."

// GiveUp ends a press that could not be processed in time: the person is told to press again, and the
// press is deleted, so that the timeout sweep, which leaves an interaction with a press waiting, can go on
// with it. Telling is best-effort. An error means the press is still recorded.
func (p *Pipeline) GiveUp(ctx context.Context, press store.Press) error {
	var in *store.Interaction
	text := textGaveUp
	if row, err := p.cfg.Store.GetInteraction(ctx, press.InteractionID); err == nil {
		in, text = &row, wordsOf(requestKind(row)).gaveUp
	}
	_, err := p.finish(ctx, press, in, text)
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
