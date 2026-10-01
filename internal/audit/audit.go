// Package audit is the typed writer over the audit store (spec G8, sections 8.4, 8.5, 10).
//
// The router, the session manager and the interaction flow call it, so no caller builds a
// store.AuditEntry by hand. It takes identifiers and typed outcomes, never payloads. Every
// string that reaches a row is cleaned, redacted and capped. Identifiers, and an outcome, which is
// tags and identifiers (the task IDs of a duplicate run among them), go through redact.Secrets, the
// shapes of a token. Free text, which is an unusable pause's reason, an error and the failure log's
// line, goes through redact.Text too, which also takes the value of a "--password=x", a connection
// string, a key block or a URL's credentials. An identifier does not: an agent chooses its task IDs,
// and the rules would redact one that is 40 characters long, or holds "token=". This repeats the
// store's own redaction (every field but Email, after WP-23c) on purpose: only this package also
// deletes invisible characters and unifies spaces first. Returned errors and the failure log are
// cleaned the same way.
//
// Failure policy (spec 8.6 step 7). No write is dropped silently: each failure is logged
// (without the entry), counted through Options.OnFailure, and returned. The caller then
// follows the spec. A record that authorizes an action fails the action: Event before the
// event is queued (the webhook answers 5xx, so Alertmanager retries) and Decision before
// the answer is sent. A record of something already done (AgentCall, UnusablePause and
// its cancel, UnusablePauseCancel) is logged, metered and returned, and never blocks the
// follow-up, above all the cancel.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	maxField  = 256 // longest identifier kept, in bytes
	maxReason = 120 // longest agent-derived reason kept, in bytes
	maxError  = 300 // longest returned error text, in bytes
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
	SourceSlack Source = "slack_message"
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
}

// Options configures a Writer. The zero value logs to slog.Default, meters nothing, uses
// the wall clock and gives each write five seconds.
type Options struct {
	Logger *slog.Logger
	// OnFailure is called once for each failed write, for a metric (spec D34, section 11).
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

// New returns a Writer over log.
func New(log store.Audit, opts Options) *Writer {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	return &Writer{log: log, opts: opts}
}

// Event records an inbound alert or Slack message and what the router did with it.
func (w *Writer) Event(ctx context.Context, a Actor, src Source, eventID string, outcome EventOutcome) error {
	switch outcome {
	case EventAccepted, EventDeduped, EventRateLimited, EventDailyCapped, EventDenied:
	default:
		return fail(store.ErrInvalid, "audit event %q: unknown outcome %q", eventID, outcome)
	}
	if a.Binding == "" {
		return fail(store.ErrInvalid, "audit event %q: a Binding is required", eventID)
	}
	return w.append(ctx, store.AuditEvent, a, w.opts.Now(), clean(eventID, maxField), string(src)+": "+string(outcome))
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
	case DecisionAnswered, DecisionIncomplete, DecisionDenied, DecisionSuperseded, DecisionTimedOut, DecisionAutoDenied:
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
		} else {
			outcome += " questions=" + strconv.Itoa(len(d.Answer.Answers))
		}
	}
	at := d.At
	if at.IsZero() {
		at = w.opts.Now()
	}
	subj := "interaction=" + clean(d.InteractionID, maxField) + " task=" + clean(string(d.TaskID), maxField)
	return w.append(ctx, store.AuditInteractionDecision, a, at, subj, outcome)
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
		if w.opts.OnFailure != nil {
			w.opts.OnFailure(kind)
		}
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
		{agent.ErrOutcomeUnknown, "outcome_unknown"}, {agent.ErrStreamLost, "stream_lost"}, {agent.ErrBusy, "busy"},
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

// gluedBearer catches what redact.Secrets, which needs a gap after "bearer", misses:
// a colon separator ("Bearer: X"), and a token glued to the scheme after a format character
// (a zero-width space, say) vanished. The glued form needs a run of 8 or more characters
// starting with a letter or digit, so an identifier such as "ns/bearer-gateway" survives.
// The store now treats Unicode gaps as whitespace itself (WP-23c); the cleaning here is
// defence in depth, and the store holds without it.
var gluedBearer = regexp.MustCompile(`(?i)bearer(?::\s*\S+|[A-Za-z0-9]\S{7,})`)

// clean makes s safe for an identifier field: it deletes format characters, turns control
// characters and every Unicode space into an ASCII space, redacts the shapes of a token
// (redact.Secrets), and caps s at limit bytes, ending in "..." when it cut. The order matters.
// Format characters are deleted and spaces unified first, as defence in depth: redact.Secrets
// handles Unicode gaps on its own, and the redaction does not depend on this step. Redaction runs
// before the cut, so a token cannot be cut short of its pattern.
func clean(s string, limit int) string {
	return capped(gluedBearer.ReplaceAllString(redact.Secrets(unify(s)), "[redacted]"), limit)
}

// cleanFree is clean for free text: an error, or the reason a dialect gave for an unusable pause. It
// redacts with every rule of redact.Text, which also takes the value of a "--password=x", a
// connection string, a key block or a URL's credentials. Identifiers do not take it, and nor does an
// outcome, which is tags and identifiers: it would read the identity "secret:ns/name" as a secret
// named "secret", and redact a task ID that is 40 characters long or holds "token=".
func cleanFree(s string, limit int) string {
	return capped(gluedBearer.ReplaceAllString(redact.Text(unify(s)), "[redacted]"), limit)
}

// unify deletes format characters and turns control characters and every Unicode space into a space.
func unify(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case unicode.IsControl(r) || unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s)
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
