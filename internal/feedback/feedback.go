// Package feedback is the useful and not-useful buttons of an investigation's final message (spec 8.7): the buttons, as
// parts of the chat-neutral model, and the processing of a press of one. A person who is a member of the channel, and is
// no bot, has one vote on each message, which the latest press replaces; the vote is stored with the Binding, the agent and
// the alert subject when the origin came from an alert trigger, audited by identifiers and verdict, counted, and acknowledged
// to the voter alone.
//
// The press takes the path of a prompt's: it is recorded before Slack is acknowledged (store.Presses, which survive a
// restart), and a worker (interact.Worker) processes it off the acknowledgement path, where the checks of who pressed, which
// are calls to the platform, may take their time. The platform is named nowhere here: the buttons are chat.Buttons and a
// press is a chat.Interaction.
package feedback

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// ErrNotFeedback is returned by Press for a button whose value is not a feedback value: it is someone else's, for the
// router to hand on.
var ErrNotFeedback = errors.New("feedback: the pressed button is not a feedback button")

const (
	valueVersion = "fb1"
	valueSep     = "|"
	lanePrefix   = valueVersion + valueSep

	// MaxAlertLabels is how many distinct alert names a Binding has a series of its own for in
	// agentkourier_investigation_feedback_total: the votes on any other name are counted under "other". The names of a
	// Binding whose trigger allowlists its alerts are few; this is the bound for one that does not.
	MaxAlertLabels = 100

	// maxAlertName bounds a name kept as the alert subject of a trigger session, as a prompt's value is (render.checkName).
	maxAlertName = 128
)

// Fixed texts, told to the voter alone. None holds anything the agent or an alert wrote.
const (
	textGone   = "This message can no longer be rated."
	textGaveUp = "Couldn't record your vote. Please press the button again."
)

// The label values of an alert name that is none: a message that is no alert's, and an alert that has no name.
const (
	labelNone    = "none"
	labelUnknown = "unknown"
	labelOther   = "other"
)

// unknownAgent is the agent of a vote on a session that holds none: one made by a build from before the agent was kept.
const unknownAgent = "unknown"

// Value is what a feedback button holds: the kind of message it is on, the verdict it gives, the thread the message is in
// (the session, with the connection and channel of the press) and the turn whose output the message is. It says nothing a
// person or an agent wrote, and it is all the processing needs to find what is rated: the thread is in it, and not left to the
// platform's press, which may not say it.
type Value struct {
	Kind    store.FeedbackKind
	Verdict store.Verdict
	Thread  string
	Turn    string
}

// String is the value as the button holds it: "fb1|kind|verdict|thread|turn", the turn last and taken verbatim.
func (v Value) String() string {
	return strings.Join([]string{valueVersion, string(v.Kind), string(v.Verdict), v.Thread, v.Turn}, valueSep)
}

// ParseValue reads what String made, and says false for anything else, a value of another version included.
func ParseValue(s string) (Value, bool) {
	p := strings.SplitN(s, valueSep, 5)
	if len(p) != 5 || p[0] != valueVersion || p[3] == "" || p[4] == "" {
		return Value{}, false
	}
	v := Value{Kind: store.FeedbackKind(p[1]), Verdict: store.Verdict(p[2]), Thread: p[3], Turn: p[4]}
	return v, v.Kind.Valid() && v.Verdict.Valid()
}

// maxIDs is the most that the thread and the turn of a button take of its value, in bytes. A button's value has a bound of its
// own on a platform (2000 characters on Slack), and a message that the platform refuses is lost whole: the buttons are left off
// a message that is named by more than any ID Agent Kourier makes (a message ID is about 40 bytes), and never cost its text.
const maxIDs = 500

// Parts are the parts that a final message of the kind, the output of turn in thread, gets under its text: the question and the
// two buttons. A message in no thread, or whose IDs are too long for a button's value, gets none.
func Parts(kind store.FeedbackKind, thread, turn string) []chat.Part {
	if thread == "" || turn == "" || len(thread)+len(turn) > maxIDs {
		return nil
	}
	button := func(id, label string, verdict store.Verdict) chat.Button {
		return chat.Button{ID: id, Label: label, Value: Value{Kind: kind, Verdict: verdict, Thread: thread, Turn: turn}.String()}
	}
	return []chat.Part{
		chat.Context{Text: "Was this useful?"},
		chat.Buttons{Buttons: []chat.Button{
			button("feedback-useful", "Useful", store.VerdictUseful),
			button("feedback-not-useful", "Not useful", store.VerdictNotUseful),
		}},
	}
}

// AlertName returns raw when it has the shape of an alert name, ASCII letters
// and digits and "_", ".", "-" and ":", at most 128 bytes (the shape a
// prompt's values must have, spec R2), and otherwise returns empty. It is text
// out of a trigger, so a sentence, a line break or a byte that Postgres text
// cannot hold is never used as feedback attribution.
func AlertName(raw string) string {
	if len(raw) > maxAlertName {
		return ""
	}
	for i := range len(raw) {
		if !nameByte(raw[i]) {
			return ""
		}
	}
	return raw
}

// nameByte says whether c may be in a name: an ASCII letter or digit, or one of "_", ".", "-" and ":".
func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-:", c) >= 0
}

// IsPress says whether press is a feedback press, which Handler.Process works, and not an interaction's.
func IsPress(p store.Press) bool { return strings.HasPrefix(p.InteractionID, lanePrefix) }

// Store is what the handler keeps and reads: the presses waiting, the votes, and the outbox rows that say which message a turn's
// output is.
type Store interface {
	store.Presses
	store.Feedback
	GetOutbox(ctx context.Context, id string) (store.OutboxEntry, error)
}

// Auditor records a vote: *audit.Writer.
type Auditor interface {
	FeedbackVote(ctx context.Context, a audit.Actor, v audit.FeedbackVote) error
}

// Config configures a Handler. Everything but Now, Logger, Limits and Notify is required.
type Config struct {
	Store      Store
	Sessions   interact.SessionReader
	Authorizer interact.Authorizer
	Ephemerals interact.Ephemeral
	Auditor    Auditor
	Now        func() time.Time // time.Now if nil
	Logger     *slog.Logger     // slog.Default() if nil
	// Limits bounds the presses waiting (default 1000 in all, 10 for one person).
	Limits store.PressLimits
	// Notify is called with each press recorded, for the worker to pick it up. Required. It runs in Press, on the path that
	// acknowledges the button to Slack, so it must never block (interact.Worker.Submit does not).
	Notify func(store.Press)
}

// Handler takes the presses of feedback buttons. Press is the sink's side and Process the worker's. It is safe for
// concurrent use.
type Handler struct {
	cfg Config

	mu     sync.Mutex
	labels map[string]map[string]struct{} // by Binding: the alert names that have a series of their own
}

var _ interact.PressProcessor = (*Handler)(nil)

// New returns a Handler over cfg.
func New(cfg Config) (*Handler, error) {
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Authorizer == nil || cfg.Ephemerals == nil || cfg.Auditor == nil {
		return nil, errors.New("feedback: Store, Sessions, Authorizer, Ephemerals and Auditor are required")
	}
	if cfg.Notify == nil {
		return nil, errors.New("feedback: Notify is required")
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
	return &Handler{cfg: cfg, labels: map[string]map[string]struct{}{}}, nil
}

// pressInputs is what a press records beside the person and the message: the connection it came in on, which a press row has
// no column for, and the button's value, whose verdict the lane leaves out.
type pressInputs struct {
	Conn  string `json:"conn"`
	Value string `json:"value"`
}

// laneOf is the lane a feedback press is worked in: both buttons of a message, so that a person's presses are processed in the
// order they were made and the later one wins. It starts with lanePrefix, which no interaction's ID does.
func laneOf(turn string) string { return lanePrefix + turn }

// Press records a press of a feedback button and returns, at Agent Kourier's own clock. It returns ErrNotFeedback for a button that
// is not one, and store.ErrLimit when too many presses wait. A press that names no person or message is acknowledged and
// dropped.
func (h *Handler) Press(ctx context.Context, i chat.Interaction) error {
	v, ok := ParseValue(i.PromptID)
	if !ok {
		return ErrNotFeedback
	}
	if i.User == "" || i.At.Channel == "" || i.At.ID == "" {
		h.cfg.Logger.WarnContext(ctx, "feedback: a press that names no person or message was dropped", "channel", i.At.Channel, "message", i.At.ID)
		return nil
	}
	inputs, err := json.Marshal(pressInputs{Conn: i.At.Conn, Value: i.PromptID})
	if err != nil {
		return fmt.Errorf("feedback: encode a press: %w", err)
	}
	press := store.Press{InteractionID: laneOf(v.Turn), User: i.User, Channel: i.At.Channel, MessageRef: i.At.ID, Inputs: inputs, PressedAt: h.cfg.Now()}
	if press.Seq, err = h.cfg.Store.RecordPress(ctx, press, h.cfg.Limits); err != nil {
		return fmt.Errorf("feedback: record a press on %s: %w", v.Turn, err)
	}
	h.cfg.Notify(press)
	return nil
}

// readAll is a threadTTL no session outlives: a message can be rated for as long as its session is stored.
const readAll = 100 * 365 * 24 * time.Hour

// Process works one recorded press through: find the session of its message and the Binding's agent, check who pressed (a
// member of the channel, and no bot), store the vote, and tell the voter. The press of a bot, or of a person outside the
// channel, is dropped without a word (the platform would refuse a word to the second), and a message whose session or Binding
// is gone is told so. The
// vote is audited, counted and acknowledged only when it changes what was stored: pressing the button of the verdict that is
// recorded already says that, and counts nothing. A failure to read, to check or to store is an error and leaves the press
// for another try, and only the worker's cap ends it (GiveUp). The audit, the count and the word to the voter come after
// the vote is stored and never undo it: a failure of the audit is logged and metered by its writer, and a person who is not
// told can look at the buttons or press again.
func (h *Handler) Process(ctx context.Context, press store.Press) (interact.Result, error) {
	var in pressInputs
	v, ok := Value{}, false
	if err := json.Unmarshal(press.Inputs, &in); err == nil {
		v, ok = ParseValue(in.Value)
	}
	if !ok {
		h.cfg.Logger.WarnContext(ctx, "feedback: a recorded press cannot be read; it was dropped", "channel", press.Channel)
		return h.finish(ctx, press, chat.Target{}, "")
	}
	key := store.ConversationKey{Connection: in.Conn, Channel: press.Channel, Thread: v.Thread}
	to := chat.Target{Conn: in.Conn, Channel: press.Channel, Thread: v.Thread}

	sess, err := h.cfg.Sessions.GetSession(ctx, key, readAll, h.cfg.Now())
	if errors.Is(err, store.ErrNotFound) {
		return h.finish(ctx, press, to, textGone)
	} else if err != nil {
		return interact.Retry, fmt.Errorf("feedback: read the session of %s: %w", v.Turn, err)
	}
	// The message the buttons are on is the one the turn's outbox row says it is, in the channel of the press: the thread and
	// the turn in the value are the button's own words, and a vote is recorded for them only if the platform agrees (as a
	// prompt's press must name the message of its interaction).
	switch ok, err := h.onMessage(ctx, press, v); {
	case err != nil:
		return interact.Retry, fmt.Errorf("feedback: read the message of %s: %w", v.Turn, err)
	case !ok:
		h.cfg.Logger.DebugContext(ctx, "feedback: a press names a message that is not the turn's; it was ignored", "channel", press.Channel)
		return h.finish(ctx, press, to, "")
	}
	agent := cmp.Or(sess.Agent, unknownAgent) // a session of a build from before the agent was kept has none

	switch member, err := h.cfg.Authorizer.IsMember(ctx, press.Channel, press.User); {
	case err != nil:
		return interact.Retry, fmt.Errorf("feedback: check who pressed on %s: %w", v.Turn, err)
	case !member: // told of nothing: Slack refuses an ephemeral to a person who is not in the channel
		h.cfg.Logger.DebugContext(ctx, "feedback: a press of a person outside the channel was ignored", "channel", press.Channel)
		return h.finish(ctx, press, to, "")
	}
	if bot, err := h.cfg.Authorizer.IsBot(ctx, press.User); err != nil {
		return interact.Retry, fmt.Errorf("feedback: check who pressed on %s: %w", v.Turn, err)
	} else if bot {
		return h.finish(ctx, press, to, "")
	}

	alert := alertName(sess.Origin)
	previous, err := h.cfg.Store.RecordVote(ctx, store.FeedbackVote{
		Key: key, Turn: v.Turn, User: press.User, Binding: sess.Binding, Agent: agent, Alert: alert,
		Kind: v.Kind, Verdict: v.Verdict, At: press.PressedAt,
	})
	if err != nil {
		return interact.Retry, fmt.Errorf("feedback: store the vote on %s: %w", v.Turn, err)
	}
	if previous == v.Verdict {
		return h.finish(ctx, press, to, "Your vote is already recorded as "+words(v.Verdict)+".")
	}
	actor := audit.Actor{User: press.User, Binding: sess.Binding}
	// A failed audit write is metered by the writer; the vote is stored, and stays, and the failure is logged here.
	if err := h.cfg.Auditor.FeedbackVote(ctx, actor, audit.FeedbackVote{Turn: v.Turn, Channel: press.Channel, Thread: v.Thread, Verdict: v.Verdict, Changed: previous != ""}); err != nil {
		h.cfg.Logger.WarnContext(ctx, "feedback: a vote was not audited", "binding", sess.Binding, "err", err)
	}
	telemetry.Count(ctx, "agentkourier_investigation_feedback_total", sess.Binding, agent, h.label(sess.Binding, v.Kind, alert), string(v.Verdict))
	return h.finish(ctx, press, to, "Thanks, your vote is recorded as "+words(v.Verdict)+". To change it, press the other button.")
}

// alertName maps the generic session origin to the feedback package's legacy alert attribution. The
// botMessage source carries a validated alertname subject from the trigger adapter, and legacy is the
// migration's historical source. alertmanager is no longer written: sessions created before trigger presets
// were removed carry it as their origin source, and their votes still attribute to the alert. A subject on
// another source remains generic metadata.
func alertName(origin store.Origin) string {
	if origin.Source != "alertmanager" && origin.Source != "botMessage" && origin.Source != "legacy" {
		return ""
	}
	return AlertName(origin.Subject)
}

// onMessage says whether the message that press was made on is the output of v's turn in v's thread, in the channel of the press:
// the outbox row of the turn's reply, or of its reply posted again (outbox.RepostedSource), holds that message and place. An
// error is a read that failed, and says nothing of the press.
func (h *Handler) onMessage(ctx context.Context, press store.Press, v Value) (bool, error) {
	for _, source := range []string{v.Turn, outbox.RepostedSource(v.Turn)} {
		row, err := h.cfg.Store.GetOutbox(ctx, outbox.ID(source, store.OutboxReply))
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return false, err
		}
		if row.MessageRef != "" && row.MessageRef == press.MessageRef && row.Channel == press.Channel && row.Thread == v.Thread {
			return true, nil
		}
	}
	return false, nil
}

// words is a verdict as the voter reads it.
func words(v store.Verdict) string {
	if v == store.VerdictUseful {
		return "useful"
	}
	return "not useful"
}

// label is the alertname label of a vote on a message of kind in a thread of Binding that was started for alert: the name,
// if the Binding has fewer than MaxAlertLabels others, and else "other"; "unknown" for an investigation whose alert has no
// name, and "none" for an answer in a thread that no alert started.
func (h *Handler) label(binding string, kind store.FeedbackKind, alert string) string {
	switch {
	case alert == "" && kind == store.FeedbackInvestigation:
		return labelUnknown
	case alert == "":
		return labelNone
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	names := h.labels[binding]
	if names == nil {
		names = map[string]struct{}{}
		h.labels[binding] = names
	}
	if _, ok := names[alert]; !ok {
		if len(names) >= MaxAlertLabels {
			return labelOther
		}
		names[alert] = struct{}{}
	}
	return alert
}

// GiveUp ends a press that could not be processed in time: the person is told to press again, and the press is deleted.
// Telling is best-effort. An error means the press is still recorded.
func (h *Handler) GiveUp(ctx context.Context, press store.Press) error {
	var in pressInputs
	_ = json.Unmarshal(press.Inputs, &in)
	v, _ := ParseValue(in.Value) // a press that cannot be read is told in the channel's own place: no thread
	_, err := h.finish(ctx, press, chat.Target{Conn: in.Conn, Channel: press.Channel, Thread: v.Thread}, textGaveUp)
	return err
}

// finish tells the person text, if there is any, in the thread of to, and deletes the press. Telling is best-effort: a
// person who is not told can press again, and a failure is only logged.
func (h *Handler) finish(ctx context.Context, press store.Press, to chat.Target, text string) (interact.Result, error) {
	if text != "" {
		if err := h.cfg.Ephemerals.Ephemeral(ctx, to, press.User, chat.Message{Fallback: text}); err != nil {
			h.cfg.Logger.WarnContext(ctx, "feedback: a person could not be told the outcome of their press", "channel", press.Channel, "err", err)
		}
	}
	if err := h.cfg.Store.DeletePress(ctx, press.Seq); err != nil {
		return interact.Retry, fmt.Errorf("feedback: delete the press %d: %w", press.Seq, err)
	}
	return interact.Done, nil
}
