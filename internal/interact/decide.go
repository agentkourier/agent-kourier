package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// The fixed texts told to a person alone. None holds anything the agent wrote.
const (
	textLate      = "This question has timed out, so your answer was not taken."
	textInvalid   = "That answer could not be used. Please check it and press Submit again."
	textTimedOut  = "This question has timed out."
	textClosed    = "This question is closed."
	textFirstNone = "Someone else answered this question first."
	// textFirstWords follows the mention of the one who did, in the message that tells the loser.
	textFirstWords = "answered this question first."
)

// Ephemeral shows a message to one person alone: a chat.Platform.
type Ephemeral interface {
	Ephemeral(ctx context.Context, t chat.Target, user string, m chat.Message) error
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

// textClosedBefore tells the thread that an answer reached a task that was no longer waiting for it. It is
// neutral: courier does not know whether the question timed out, was stopped or was left behind. The
// decider and the backstop use it alike, so one answer is never told two ways.
const textClosedBefore = "Your answer was not sent: the question closed before it could be."

// PromptReplacer takes the controls off the prompt of an interaction that has ended: *Replacer.
type PromptReplacer interface {
	Replace(ctx context.Context, id string) error
}

// DecideConfig configures a Decider. Everything but Logger and OnDecision is required.
type DecideConfig struct {
	Store      store.Interactions
	Auditor    Auditor
	Ephemerals Ephemeral
	// Session takes the answers, and Outbox the notice for one that arrives too late. Sessions is read
	// to tell a task that has ended from one the session has not parked on yet.
	Session  Session
	Sessions SessionReader
	Outbox   Enqueuer
	// Replacer, if set, takes the controls off the prompt once the answer has been sent.
	Replacer PromptReplacer
	Now      func() time.Time // time.Now if nil
	Logger   *slog.Logger     // slog.Default() if nil
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
	if cfg.Store == nil || cfg.Auditor == nil || cfg.Ephemerals == nil || cfg.Session == nil || cfg.Sessions == nil || cfg.Outbox == nil {
		return nil, errors.New("interact: Store, Auditor, Ephemerals, Session, Sessions and Outbox are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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
// it, send it. Building comes first because it is validation and not an action: an answer the
// dialect refuses is told to the person, and nothing is recorded, so no interaction is left answered
// with an answer that can never be sent. The audit comes before the decision takes effect, and a failed
// audit write stops it (spec 8.6 step 7): the error is returned, and the press, which stays recorded, is
// decided again. It returns nil when the press is decided for good, whichever way it came out. A send
// that fails is an error too: the answer is recorded, so the press, decided again, is the same person's
// retry, which sends it again without a second decision.
//
// The audit is at least once, and written before the decision takes effect, so it can be ahead of what
// happened. If the sweep, or another process, decides in the window between the read and the write, the
// press has an `answered` row for a decision that never took effect, followed by `superseded`. A crash
// between the audit and the write audits `answered` again when the press is decided again. Both are
// accepted over a decision that takes effect unaudited.
//
// The interaction is recorded as answered at PressedAt, so a press made before the deadline wins even if
// it is decided after it. One made at or after the deadline is no decision: it is told so and not audited.
func (d *Decider) Decide(ctx context.Context, pd PressDecision) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.interaction.decide", attribute.String("courier.interaction.id", pd.Interaction.ID), attribute.String("courier.interaction.kind", string(pd.Interaction.Kind)), attribute.String("courier.binding", pd.BindingName))
	telemetry.Link(ctx, pd.Interaction.TraceParent, pd.Interaction.TraceState)
	defer func() { op.Finish(telemetryErr) }()
	row := pd.Interaction
	msg, err := AnswerMessage(pd.Binding.Dialect, row.ID, pd.In, pd.Answer, pd.User)
	if err != nil {
		d.cfg.Logger.InfoContext(ctx, "interact: the dialect refused an answer", "interaction", row.ID, "err", err)
		d.tell(ctx, row, pd.User, chat.Message{Fallback: textInvalid})
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
	case again: // a retry: the decision was taken and counted before
	case late:
		d.tell(ctx, row, pd.User, chat.Message{Fallback: textLate})
		d.count("late")
		return nil
	case lost:
		d.tell(ctx, row, pd.User, lostMessage(cur))
		d.count("superseded")
		return nil
	case gone:
		return nil
	}
	if err := d.send(ctx, pd, msg, v == again); err != nil {
		return err
	}
	d.replacePrompt(ctx, row.ID)
	return nil
}

// replacePrompt takes the controls off the prompt. The decision is done and the answer sent, so a failed
// edit is logged and left to the repair, and never fails the decision: deciding it again would send the
// answer again. It does Slack I/O, and a press is decided on the worker, not on Slack's acknowledgement.
func (d *Decider) replacePrompt(ctx context.Context, id string) {
	if d.cfg.Replacer == nil {
		return
	}
	if err := d.cfg.Replacer.Replace(ctx, id); err != nil {
		d.cfg.Logger.WarnContext(ctx, "interact: the prompt of an answered interaction keeps its controls until it is repaired", "interaction", id, "err", err)
	}
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

// send gives the answer to the session, which streams what follows to the thread.
//
// A task that is not paused cannot take it. If the thread's session no longer records the task, it has
// ended or moved on: the thread is told, once, since the notice's ID comes from the interaction. If it
// still records it, the session may only not have parked on it yet (right after a restart, before the
// recovery has re-emitted the pause), and the answer is likely to be resent from its record then: a
// retryable error is returned, so the press is decided again, and the notice is not posted, since it
// would say the answer was not sent. A retry that is refused again says nothing either: it cannot be
// known whether the answer got through before a crash.
//
// store.ErrInvalid is final: no retry would change it, so it is logged and the press is done. Any other
// failure is returned, for the press to be decided again.
func (d *Decider) send(ctx context.Context, pd PressDecision, msg a2a.Message, retry bool) error {
	row := pd.Interaction
	err := d.cfg.Session.Answer(ctx, row.Key, &msg)
	switch {
	case err == nil:
		d.markDelivered(ctx, row.ID)
		return nil
	case errors.Is(err, store.ErrInvalid):
		d.cfg.Logger.ErrorContext(ctx, "interact: the session refused an answer for good, and it was not sent", "interaction", row.ID, "err", err)
		return nil
	case !errors.Is(err, session.ErrNotPaused):
		return fmt.Errorf("interact: send the answer to %s: %w", row.ID, err)
	}
	d.cfg.Logger.InfoContext(ctx, "interact: an answer found its task not paused", "interaction", row.ID, "task", row.TaskID, "retry", retry)
	if retry {
		return nil
	}
	sess, serr := d.cfg.Sessions.GetSession(ctx, row.Key, readAll, d.cfg.Now())
	switch {
	case serr == nil && sess.TaskID == row.TaskID:
		return fmt.Errorf("interact: the session is not paused on %s yet: %w", row.TaskID, err)
	case serr != nil && !errors.Is(serr, store.ErrNotFound):
		return fmt.Errorf("interact: read the session of %s: %w", row.ID, serr)
	}
	if _, err := d.cfg.Outbox.Enqueue(ctx, outbox.Post{Source: row.ID + "/answer", Role: store.OutboxNotice, Channel: row.Key.Channel,
		Thread: row.Key.Thread, Out: chat.Say(render.Notice(textClosedBefore, ""))}); err != nil {
		return fmt.Errorf("interact: tell the thread that the answer to %s came too late: %w", row.ID, err)
	}
	return nil
}

// markDelivered records that the session took the answer, which is all that tells an answer that arrived
// from one that did not (WP-33d4). The answer is sent and the decision is done, so a failed mark is
// logged and never fails the decision: deciding it again would send the answer again. The backstop may
// then tell the thread, wrongly, that the answer did not arrive, if the task ends.
func (d *Decider) markDelivered(ctx context.Context, id string) {
	if err := d.cfg.Store.MarkInteractionDelivered(ctx, id, d.cfg.Now()); err != nil {
		d.cfg.Logger.ErrorContext(ctx, "interact: an answer was sent and could not be marked delivered", "interaction", id, "err", err)
	}
}

// lostMessage says why a press did not decide the question. When another answer did, it names who: a mention of the
// one who decided, which the adapter makes only of an ID of its platform's shape and otherwise shows as "someone".
func lostMessage(cur store.Interaction) chat.Message {
	switch {
	case cur.State == store.InteractionTimedOut:
		return chat.Message{Fallback: textTimedOut}
	case cur.State != store.InteractionAnswered:
		return chat.Message{Fallback: textClosed}
	}
	return chat.Message{Fallback: textFirstNone, Parts: []chat.Part{chat.Mention{User: cur.Decider, Text: textFirstWords}}}
}

// tell shows m to the person alone. It is told once and kept nowhere, so a failure is only logged.
func (d *Decider) tell(ctx context.Context, row store.Interaction, user string, m chat.Message) {
	to := chat.Target{Conn: row.Key.Connection, Channel: row.Key.Channel, Thread: row.Key.Thread}
	if err := d.cfg.Ephemerals.Ephemeral(ctx, to, user, m); err != nil {
		d.cfg.Logger.WarnContext(ctx, "interact: a person could not be told the outcome of their press", "interaction", row.ID, "err", err)
	}
}

func (d *Decider) count(result string) {
	if d.cfg.OnDecision != nil {
		d.cfg.OnDecision(result)
	}
}
