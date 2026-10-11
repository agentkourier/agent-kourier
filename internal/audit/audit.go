// Package audit is the typed writer over the audit store (spec G8, sections 8.4, 8.5, 10).
//
// The router, the session manager and the interaction flow call it, so no caller builds a
// store.AuditEntry by hand. It takes identifiers and typed outcomes, never payloads. Every
// string that reaches a row is normalized, redacted and capped, by redact: the invisible characters are
// deleted before any secret is matched. Identifiers, and an outcome, which is tags and identifiers (the
// task IDs of a duplicate run among them), go through redact.IdentifierText, on one line, with the shapes
// of a token taken out. Free text, which is an unusable pause's reason, an error and the failure log's
// line, goes through redact.FreeText too, which also takes the value of a "--password=x", a connection
// string, a key block or a URL's credentials, and is then put on one line. An identifier does not: an
// agent chooses its task IDs, and the rules would redact one that is 40 characters long, or holds
// "token=". This repeats the store's own redaction (every field but Email) on purpose. Returned errors
// and the failure log are cleaned the same way.
//
// Failure policy (spec 8.6 step 7). No write is dropped silently: each failure is logged
// (without the entry), counted through Options.OnFailure, and returned. The caller then
// follows the spec. A record that authorizes an action fails the action: Event before the
// event is queued (an event whose record cannot be written is not queued; nothing in
// production calls Event today) and Decision before the answer is sent. A record of something already done (AgentCall, UnusablePause and
// its cancel, UnusablePauseCancel) is logged, metered and returned, and never blocks the
// follow-up, above all the cancel.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	maxToolName = 32  // longest tool name kept in a decision row, in bytes
	maxField    = 256 // longest identifier kept, in bytes
	maxReason   = 120 // longest agent-derived reason kept, in bytes
	maxError    = 300 // longest returned error text, in bytes
)

// Actor is who an entry is attributed to, where known (spec 8.5): the Slack user, their
// email, and the Binding's service identity together. Any field may be empty except
// that an Event needs Binding.
type Actor struct {
	User     string // Slack user ID
	Email    string
	Identity string // Binding service identity the action ran as
	Binding  string
}

// Source is where an inbound event came from.
type Source string

// Event sources.
const (
	SourceAlert Source = "alert"
	SourceChat  Source = "slack_message" // a chat message; the stored value keeps the name of the platform it began on
)

// EventOutcome is what the router did with an inbound event.
type EventOutcome string

// Event outcomes.
const (
	EventAccepted    EventOutcome = "accepted"
	EventDeduped     EventOutcome = "deduped"
	EventRateLimited EventOutcome = "rate_limited"
	EventDailyCapped EventOutcome = "daily_capped"
	EventDenied      EventOutcome = "denied"
)

// Op is the agent call an AgentCall entry records.
type Op string

// Agent operations.
const (
	OpSend     Op = "send"
	OpAnswer   Op = "answer"
	OpCancel   Op = "cancel"
	OpFindTask Op = "find_task"
)

// Call is one agent call and how it ended.
type Call struct {
	Op Op
	// TaskID, ContextID and MessageID identify the call; at least one is required.
	// A StreamLostError in Err supplies the task and context when they are empty.
	TaskID    a2a.TaskID
	ContextID string
	MessageID string
	// Err is the call's error, or nil. Only its type is recorded: the agent sentinels
	// (ErrNotSent, ErrOutcomeUnknown, ErrStreamLost, ErrCredentialsRejected, ...) and an
	// HTTP status code.
	Err error
	// Holders is every task FindTask returned for OpFindTask. More than one is a
	// duplicate run.
	Holders []a2a.TaskID
}

// DecisionResult is how an interaction was decided (spec 8.4).
type DecisionResult string

// Decision results.
const (
	DecisionAnswered   DecisionResult = "answered"
	DecisionIncomplete DecisionResult = "incomplete"
	DecisionDenied     DecisionResult = "denied" // not a channel member, or a bot
	DecisionSuperseded DecisionResult = "superseded"
	DecisionTimedOut   DecisionResult = "timed_out"
	DecisionAutoDenied DecisionResult = "auto_denied" // tool approval on a read-only Binding
	// DecisionAnswerCleared is an answered interaction whose pause came again after retention had cleared the
	// answer: it cannot be sent again, and the task is cancelled.
	DecisionAnswerCleared DecisionResult = "answer_cleared"
)

// Decision is a person's (or the timeout's) decision on an interaction.
type Decision struct {
	InteractionID string
	TaskID        a2a.TaskID
	Result        DecisionResult
	// At is when it was decided; zero means now.
	At time.Time
	// Answer is the submitted answer, or nil. Only its shape is recorded: counts, never
	// choices, free text or rejection reasons.
	Answer *dialect.Answer
	// ToolNames, for an answered tool approval, are the names of its tools, in the order of Answer.Decisions: ToolNames[i]
	// is the tool that Decisions[i] decided. They are recorded beside the counts, each with its verdict, cleaned and
	// cut to maxToolName bytes, and as many as fit in the row (see toolLists). The arguments are never recorded. There is
	// one name for each decision, or none: any other number is an error, since it would name the wrong tools.
	ToolNames []string
	// Shape, for an incomplete tool approval, is how many tools had a usable verdict. It says nothing of which, or how.
	Shape *ToolShape
}

// ToolShape is the shape of a press that left some tools of an approval undecided: Decided of Total tools had exactly one
// valid verdict.
type ToolShape struct {
	Decided, Total int
}

// Options configures a Writer. OnFailure is required (New refuses without it; NoFailure meters nothing).
// The rest default: the logger to slog.Default, the clock to the wall clock, and each write to five seconds.
type Options struct {
	Logger *slog.Logger
	// OnFailure is called once for each failed write, for a metric (spec D34, section 11). Required.
	OnFailure func(store.AuditKind)
	Now       func() time.Time
	// Timeout bounds one write, which runs detached from the caller's cancellation so
	// that a cancelled turn still gets its outcome recorded.
	Timeout time.Duration
}

// Writer records typed audit entries. It is safe for concurrent use.
type Writer struct {
	log  store.Audit
	opts Options
}

// NoFailure is the OnFailure of a Writer whose failed writes are logged and returned, and counted nowhere.
func NoFailure(store.AuditKind) {}

// New returns a Writer over log. It refuses a nil log or a nil Options.OnFailure, so that a wiring that leaves the metric
// out fails at start-up and not silently.
func New(log store.Audit, opts Options) (*Writer, error) {
	switch {
	case log == nil:
		return nil, errors.New("audit: a log (store.Audit) is required")
	case opts.OnFailure == nil:
		return nil, errors.New("audit: Options.OnFailure is required (NoFailure meters nothing)")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	return &Writer{log: log, opts: opts}, nil
}

// Event records an inbound alert or Slack message and what the router did with it.
func (w *Writer) Event(ctx context.Context, a Actor, src Source, eventID string, outcome EventOutcome) error {
	switch outcome {
	case EventAccepted, EventDeduped, EventRateLimited, EventDailyCapped, EventDenied:
	default:
		return fail(store.ErrInvalid, "audit event %q: unknown outcome %q", eventID, outcome)
	}
	switch src {
	case SourceAlert, SourceChat:
	default: // the outcome column is tags and identifiers, so a source is one of ours or nothing
		return fail(store.ErrInvalid, "audit event %q: unknown source %q", eventID, src)
	}
	if a.Binding == "" {
		return fail(store.ErrInvalid, "audit event %q: a Binding is required", eventID)
	}
	return w.append(ctx, store.AuditEvent, a, w.opts.Now(), clean(eventID, maxField), string(src)+": "+string(outcome))
}

// TriggerRefusal is a botMessage trigger's start that was refused (spec 8.2, R2): the first prompt failed its check, or
// could not be rendered, so no turn started. It names the value that failed and never holds it: the value is text
// from the alert, untrusted, and the audit log keeps identifiers and typed outcomes.
type TriggerRefusal struct {
	// MessageID is the alert message's turn ID (chat.MessageID), which names the message without its text.
	MessageID string
	// Trigger is the trigger's place in the Binding's chat.triggers.
	Trigger int
	// Ref is the value that failed as a template names it ("Fields.alertname", "Key"), and Reason why: short codes
	// from closed sets.
	Ref, Reason string
	// Len is the length of the value in bytes.
	Len int
}

// TriggerRefused records that a trigger's start was refused. It is an event of the chat source whose outcome says so,
// like the other events, and a record of something already decided: the caller follows its failure as the
// failure policy says, which is to log and carry on.
func (w *Writer) TriggerRefused(ctx context.Context, a Actor, r TriggerRefusal) error {
	if a.Binding == "" || r.MessageID == "" || r.Ref == "" || r.Reason == "" || r.Trigger < 0 || r.Len < 0 {
		return fail(store.ErrInvalid, "audit trigger refusal %q: a Binding, message, ref and reason are required, and no negative number", r.MessageID)
	}
	subj := "message=" + clean(r.MessageID, maxField) + " trigger=" + strconv.Itoa(r.Trigger)
	outcome := string(SourceChat) + ": trigger_refused ref=" + clean(r.Ref, maxField) + " reason=" + clean(r.Reason, maxField) + " len=" + strconv.Itoa(r.Len)
	return w.append(ctx, store.AuditEvent, a, w.opts.Now(), subj, outcome)
}

// FeedbackVote is a person's verdict on a message of an investigation (spec 8.7). It names the message by identifiers and
// holds the verdict, and nothing else: not the alert's name, which is text out of the alert.
type FeedbackVote struct {
	// Turn is the turn whose output is the rated message, and Channel and Thread the conversation it is in.
	Turn, Channel, Thread string
	Verdict               store.Verdict
	// Changed says the verdict replaced another one of the same person.
	Changed bool
}

// FeedbackVote records a vote, as an event whose outcome says the verdict. It is a record of something already done: the
// caller follows its failure as the failure policy says, which is to log and carry on.
func (w *Writer) FeedbackVote(ctx context.Context, a Actor, v FeedbackVote) error {
	if a.Binding == "" || a.User == "" || v.Turn == "" || v.Channel == "" || v.Thread == "" || !v.Verdict.Valid() {
		return fail(store.ErrInvalid, "audit feedback vote on %q: a Binding, user, turn, channel, thread and a verdict are required", v.Turn)
	}
	subj := "feedback turn=" + clean(v.Turn, maxField) + " channel=" + clean(v.Channel, maxField) + " thread=" + clean(v.Thread, maxField)
	return w.append(ctx, store.AuditEvent, a, w.opts.Now(), subj, "feedback: verdict="+string(v.Verdict)+" changed="+strconv.FormatBool(v.Changed))
}

// StaleCancel is how the cancel at the agent of a stale task went.
type StaleCancel string

// How the cancel of a stale task went.
const (
	StaleCancelOK        StaleCancel = "ok"         // the agent cancelled the task
	StaleCancelFailed    StaleCancel = "failed"     // the call failed or timed out; CancelErr says how
	StaleCancelNoBinding StaleCancel = "no_binding" // the Binding has left the config, so there is no agent to call
	StaleCancelSkipped   StaleCancel = "skipped"    // none was made: the Binding's agent could not be reached earlier in the same sweep
)

// StaleTask is a task that retention ended: it was recorded on a session that had been expired for good (a crash left it),
// and the session no longer records it. The audit names the task and its context and never the thread.
type StaleTask struct {
	TaskID    a2a.TaskID
	ContextID string
	// Cancel is how the cancel at the agent went. CancelErr is the error of a failed one, of which only its type is recorded.
	Cancel    StaleCancel
	CancelErr error
	// Interactions is how many pending interactions of the task were cancelled with it.
	Interactions int
}

// StaleTaskEnded records that retention ended a stale task, and how the cancel at the agent went. It records something
// already done, so a failure is logged, metered and returned, and never undoes it.
func (w *Writer) StaleTaskEnded(ctx context.Context, a Actor, t StaleTask) error {
	failed := t.Cancel == StaleCancelFailed
	switch {
	case a.Binding == "":
		return fail(store.ErrInvalid, "audit stale task %q: a Binding is required", t.TaskID)
	case t.TaskID == "":
		return fail(store.ErrInvalid, "audit stale task: a task ID is required")
	case t.Interactions < 0:
		return fail(store.ErrInvalid, "audit stale task %q: a negative number of interactions", t.TaskID)
	case failed != (t.CancelErr != nil):
		return fail(store.ErrInvalid, "audit stale task %q: exactly a failed cancel has an error", t.TaskID)
	}
	switch t.Cancel {
	case StaleCancelOK, StaleCancelFailed, StaleCancelNoBinding, StaleCancelSkipped:
	default:
		return fail(store.ErrInvalid, "audit stale task %q: unknown cancel %q", t.TaskID, t.Cancel)
	}
	cancel := string(t.Cancel)
	if t.CancelErr != nil {
		cancel += " " + strings.Join(errorTags(t.CancelErr), ",")
	}
	outcome := "stale_task_ended: cancel=" + cancel + " interactions=" + strconv.Itoa(t.Interactions)
	return w.append(ctx, store.AuditEvent, a, w.opts.Now(), subject(t.TaskID, t.ContextID, ""), outcome)
}

// AgentCall records a send, answer, cancel or FindTask and its typed outcome.
func (w *Writer) AgentCall(ctx context.Context, a Actor, c Call) error {
	var lost *agent.StreamLostError
	if errors.As(c.Err, &lost) {
		c.TaskID, c.ContextID = orElse(c.TaskID, lost.TaskID), orElse(c.ContextID, lost.ContextID)
	}
	if len(c.Holders) > 0 {
		c.TaskID = orElse(c.TaskID, c.Holders[0])
	}
	switch c.Op {
	case OpSend, OpAnswer, OpCancel, OpFindTask:
	default:
		return fail(store.ErrInvalid, "audit agent call: unknown op %q", c.Op)
	}
	subj := subject(c.TaskID, c.ContextID, c.MessageID)
	if subj == "" {
		return fail(store.ErrInvalid, "audit %s: a task, context or message ID is required", c.Op)
	}
	outcome := "ok"
	switch {
	case c.Err != nil:
		outcome = "failed " + strings.Join(errorTags(c.Err), ",")
	case c.Op == OpFindTask && len(c.Holders) == 0:
		outcome = "absent"
	case c.Op == OpFindTask && len(c.Holders) == 1:
		outcome = "found"
	case c.Op == OpFindTask:
		outcome = "duplicate_run holders=" + strconv.Itoa(len(c.Holders)) + " ids="
		room := store.MaxAuditOutcome - len(c.Op) - len(": ") - len(outcome)
		for i, h := range c.Holders {
			id := clean(string(h), maxField)
			if i > 0 {
				id = "," + id
			}
			if len(id) > room {
				break // a whole id or none: a cut id would name no task
			}
			outcome, room = outcome+id, room-len(id)
		}
	}
	return w.append(ctx, store.AuditAgentCall, a, w.opts.Now(), subj, string(c.Op)+": "+outcome)
}

// Decision records who decided an interaction, when, and the shape of the answer.
func (w *Writer) Decision(ctx context.Context, a Actor, d Decision) error {
	switch d.Result {
	case DecisionAnswered, DecisionIncomplete, DecisionDenied, DecisionSuperseded, DecisionTimedOut, DecisionAutoDenied, DecisionAnswerCleared:
	default:
		return fail(store.ErrInvalid, "audit decision %q: unknown result %q", d.InteractionID, d.Result)
	}
	outcome := string(d.Result)
	if d.Answer != nil {
		if len(d.Answer.Decisions) > 0 {
			approved := 0
			for _, x := range d.Answer.Decisions {
				if x.Approved {
					approved++
				}
			}
			outcome += fmt.Sprintf(" approved=%d denied=%d", approved, len(d.Answer.Decisions)-approved)
			if len(d.ToolNames) > 0 {
				if len(d.ToolNames) != len(d.Answer.Decisions) {
					return fail(store.ErrInvalid, "audit decision %q: %d tool names for %d decisions", d.InteractionID, len(d.ToolNames), len(d.Answer.Decisions))
				}
				outcome += toolLists(len(outcome), d.ToolNames, d.Answer.Decisions)
			}
		} else {
			outcome += " questions=" + strconv.Itoa(len(d.Answer.Answers))
		}
	}
	if d.Shape != nil {
		outcome += fmt.Sprintf(" tools_decided=%d/%d", d.Shape.Decided, d.Shape.Total)
	}
	at := d.At
	if at.IsZero() {
		at = w.opts.Now()
	}
	subj := "interaction=" + clean(d.InteractionID, maxField) + " task=" + clean(string(d.TaskID), maxField)
	return w.append(ctx, store.AuditInteractionDecision, a, at, subj, outcome)
}

// toolLists is the part of a decision row that names the tools of an answered approval with their verdicts:
// ` approved_tools=["a","b"] denied_tools=["c"]`, each list in the order of the request, a list left out when it is empty.
// Each name is cleaned, cut to maxToolName bytes and written as a quoted string, so that a name with a comma or a bracket
// in it is one name and cannot read as two tools. The row holds MaxAuditOutcome bytes in all, and 20 tools of long names
// do not fit, so the lists share what is left of the row after the counts (used bytes): a list that is short gives its
// spare room to the other, and a list that does not fit ends in `+K`, K being how many of its names are not listed. A name
// is whole or left out. The counts before it are always exact, so what is left out is told by the difference.
func toolLists(used int, names []string, decisions []dialect.Decision) string {
	var approved, denied []string
	for i, x := range decisions {
		name := strconv.Quote(clean(names[i], maxToolName))
		if x.Approved {
			approved = append(approved, name)
		} else {
			denied = append(denied, name)
		}
	}
	const unbounded = 1 << 20
	room := store.MaxAuditOutcome - used
	wantA, wantD := len(fitList("approved_tools", approved, unbounded)), len(fitList("denied_tools", denied, unbounded))
	roomA, roomD := room/2, room-room/2
	switch {
	case wantA < roomA:
		roomA, roomD = wantA, roomD+roomA-wantA
	case wantD < roomD:
		roomA, roomD = roomA+roomD-wantD, wantD
	}
	return fitList("approved_tools", approved, roomA) + fitList("denied_tools", denied, roomD)
}

// fitList is ` label=[item,item,+K]` in at most room bytes, or "" if items is empty or even `+K` does not fit. Whole items
// are listed while they fit, with `+K` reserved at the end for the K that do not; K is left out when all fit. No item
// starts with a plus, because an item is a quoted string.
func fitList(label string, items []string, room int) string {
	if len(items) == 0 {
		return ""
	}
	head, tail := " "+label+"=[", "]"
	if whole := head + strings.Join(items, ",") + tail; len(whole) <= room {
		return whole
	}
	used := len(head) + len(tail) + len(",+") + len(strconv.Itoa(len(items))) // the marker is reserved first
	if used > room {
		return ""
	}
	var kept []string
	for _, it := range items {
		if used+len(it)+1 > room {
			break
		}
		kept, used = append(kept, it), used+len(it)+1
	}
	return head + strings.Join(append(kept, "+"+strconv.Itoa(len(items)-len(kept))), ",") + tail
}

// UnusablePause records a pause the dialect could not use (spec 8.4 step 1). cause wraps
// dialect.ErrUnusablePause, whose text is fixed by the dialect and never repeats what the
// agent sent, so the reason is stored, cleaned and capped like every other string. Record
// the cancel that follows with UnusablePauseCancel.
func (w *Writer) UnusablePause(ctx context.Context, a Actor, taskID a2a.TaskID, contextID string, cause error) error {
	if cause == nil {
		return fail(store.ErrInvalid, "audit unusable pause: a cause is required")
	}
	return w.pause(ctx, a, taskID, contextID, "unusable_pause: "+cleanFree(cause.Error(), maxReason))
}

// UnusablePauseCancel records the cancel of an unusable pause: ok when cancelErr is nil,
// failed with its typed tags otherwise. Its text is never stored. A failed write here must not stop the cancel
// (spec 8.6 step 7), so call it after the cancel, whatever the write returns.
func (w *Writer) UnusablePauseCancel(ctx context.Context, a Actor, taskID a2a.TaskID, contextID string, cancelErr error) error {
	outcome := "unusable_pause_cancel: ok"
	if cancelErr != nil {
		outcome = "unusable_pause_cancel: failed " + strings.Join(errorTags(cancelErr), ",")
	}
	return w.pause(ctx, a, taskID, contextID, outcome)
}

func (w *Writer) pause(ctx context.Context, a Actor, taskID a2a.TaskID, contextID, outcome string) error {
	subj := subject(taskID, contextID, "")
	if subj == "" {
		return fail(store.ErrInvalid, "audit unusable pause: a task or context ID is required")
	}
	return w.append(ctx, store.AuditAgentCall, a, w.opts.Now(), subj, outcome)
}

// append cleans a's fields and writes the entry, detached from ctx's cancellation.
func (w *Writer) append(ctx context.Context, kind store.AuditKind, a Actor, at time.Time, subject, outcome string) error {
	e := store.AuditEntry{At: at, Kind: kind, User: clean(a.User, maxField), Email: clean(a.Email, maxField),
		Identity: clean(a.Identity, maxField), Binding: clean(a.Binding, maxField), Subject: subject, Outcome: clean(outcome, store.MaxAuditOutcome)}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.Timeout)
	defer cancel()
	if err := w.log.AppendAudit(wctx, e); err != nil {
		w.opts.Logger.ErrorContext(ctx, "audit write failed", "kind", kind, "error", cleanFree(err.Error(), maxReason))
		w.opts.OnFailure(kind)
		return fail(err, "audit %s", kind)
	}
	return nil
}

// cleanError carries a cleaned message and still unwraps to its cause, so errors.Is works
// and the text of a store error, which may quote a secret, is not returned.
type cleanError struct {
	msg   string
	cause error
}

func (e *cleanError) Error() string { return e.msg }
func (e *cleanError) Unwrap() error { return e.cause }

// fail returns "<format>: <cause>" as a cleanError. It cleans each argument before
// formatting: %q would turn an invisible rune into an ASCII escape that clean, run on the
// finished message, could no longer see, and so no longer delete or match.
func fail(cause error, format string, args ...any) error {
	for i, a := range args {
		args[i] = clean(fmt.Sprint(a), maxField)
	}
	return &cleanError{msg: cleanFree(fmt.Sprintf(format, args...)+": "+cause.Error(), maxError), cause: cause}
}

// errorTags names the typed causes in err, never its text.
func errorTags(err error) []string {
	var tags []string
	for _, t := range []struct {
		err error
		tag string
	}{
		{agent.ErrCredentialsRejected, "credentials_rejected"}, {agent.ErrNotSent, "not_sent"},
		{agent.ErrOutcomeUnknown, "outcome_unknown"}, {agent.ErrStreamLost, "stream_lost"}, {agent.ErrNotReady, "not_ready"}, {agent.ErrBusy, "busy"},
		{agent.ErrForbidden, "forbidden"}, {agent.ErrAnswerRejected, "answer_rejected"}, {agent.ErrCancelFailed, "cancel_failed"}, {agent.ErrTokenRequired, "token_required"},
		{agent.ErrTokenUnreadable, "token_unreadable"}, {agent.ErrHeaderUnreadable, "header_unreadable"},
		{agent.ErrPoolClosed, "pool_closed"}, {agent.ErrStreamingUnsupported, "streaming_unsupported"},
		// Answer's automatic cancel (R17) can fail and is joined to ErrAnswerRejected: a wedged task must not read as cleared.
		{a2a.ErrTaskNotCancelable, "task_not_cancelable"}, {a2a.ErrTaskNotFound, "task_not_found"},
		{context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline_exceeded"},
	} {
		if errors.Is(err, t.err) {
			tags = append(tags, t.tag)
		}
	}
	var hs *dialect.HTTPStatusError
	if errors.As(err, &hs) {
		tags = append(tags, "http_"+strconv.Itoa(hs.Code))
	}
	if len(tags) == 0 {
		tags = append(tags, "error")
	}
	return tags
}

// subject joins the identifiers present, or returns "" when there are none.
func subject(task a2a.TaskID, contextID, messageID string) string {
	var parts []string
	for _, p := range [][2]string{{"task", string(task)}, {"context", contextID}, {"message", messageID}} {
		if p[1] != "" {
			parts = append(parts, p[0]+"="+clean(p[1], maxField))
		}
	}
	return strings.Join(parts, " ")
}

func orElse[T ~string](have, fallback T) T {
	if have != "" {
		return have
	}
	return fallback
}

// clean makes s safe for an identifier field: redact.IdentifierText normalizes it onto one line and takes
// the shapes of a token out, and capped cuts the result to limit bytes, ending in "..." when it cut.
// Redaction runs before the cut, so a token cannot be cut short of its pattern.
func clean(s string, limit int) string {
	return capped(redact.IdentifierText(s), limit)
}

// cleanFree is clean for free text: an error, or the reason a dialect gave for an unusable pause. It
// redacts with every rule of redact.FreeTextLine, which also takes the value of a "--password=x", a
// connection string, a key block or a URL's credentials, and puts the text on one line. It reads the text
// joined as well as with its lines, so a flag, a user or a URL that a line break cuts from its value is
// taken as it was before audit redacted the text it joined. Identifiers
// do not take it, and nor does an outcome, which is tags and identifiers: it would read the identity
// "secret:ns/name" as a secret named "secret", and redact a task ID that is 40 characters long or
// holds "token=".
func cleanFree(s string, limit int) string {
	return capped(redact.FreeTextLine(s), limit)
}

// capped cuts s to limit bytes at a character boundary, ending in "..." when it cut.
func capped(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
