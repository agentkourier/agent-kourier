// Package audit is the typed writer over the audit store (spec G8, sections 8.4, 8.5, 10).
//
// The router, the session manager and the interaction flow call it, so no caller builds a
// store.AuditEntry by hand. It takes identifiers and typed outcomes, never payloads, and
// renders an outcome from the error's type, never its text, so a token or header value in
// an error message cannot reach a row. Every string that does reach a row (identifiers,
// an unusable pause's reason) is stripped of control characters, run through
// store.RedactSecrets and capped, because the store redacts only Subject and Outcome.
//
// Failure policy. The spec says every event, call and decision is audited (G8) and that
// events are never dropped silently (section 11), but is silent on a failed audit write.
// This package never drops one silently: each failure is logged (without the entry),
// counted through Options.OnFailure, and returned, wrapped. The caller decides. The
// intended split: a record that authorizes an action (Event before enqueueing, Decision
// before sending the answer) fails the operation, because an unaudited approval must not
// take effect; a record of something already done (AgentCall, UnusablePause) must not
// stop the follow-up, above all the cancel of an unusable pause.
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	maxField  = 256 // longest identifier kept, in bytes
	maxReason = 120 // longest agent-derived reason kept, in bytes
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
		return fmt.Errorf("audit event %q: unknown outcome %q: %w", eventID, outcome, store.ErrInvalid)
	}
	if a.Binding == "" {
		return fmt.Errorf("audit event %q: a Binding is required: %w", eventID, store.ErrInvalid)
	}
	return w.append(ctx, store.AuditEvent, a, w.opts.Now(), clean(eventID, maxField), string(src)+": "+string(outcome))
}

// UnusablePause records a pause the dialect could not use (cause wraps
// dialect.ErrUnusablePause, spec 8.4 step 1). Record its cancel with AgentCall and OpCancel.
func (w *Writer) UnusablePause(ctx context.Context, a Actor, taskID a2a.TaskID, contextID string, cause error) error {
	subj := subject(taskID, contextID, "")
	if subj == "" || cause == nil {
		return fmt.Errorf("audit unusable pause: a task or context ID and a cause are required: %w", store.ErrInvalid)
	}
	return w.append(ctx, store.AuditAgentCall, a, w.opts.Now(), subj, "unusable_pause: "+clean(cause.Error(), maxReason))
}

// append cleans a's fields and writes the entry, detached from ctx's cancellation.
func (w *Writer) append(ctx context.Context, kind store.AuditKind, a Actor, at time.Time, subject, outcome string) error {
	e := store.AuditEntry{At: at, Kind: kind, User: clean(a.User, maxField), Email: clean(a.Email, maxField),
		Identity: clean(a.Identity, maxField), Binding: clean(a.Binding, maxField), Subject: subject, Outcome: clean(outcome, store.MaxAuditOutcome)}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.Timeout)
	defer cancel()
	if err := w.log.AppendAudit(wctx, e); err != nil {
		w.opts.Logger.ErrorContext(ctx, "audit write failed", "kind", kind, "error", clean(err.Error(), maxReason))
		if w.opts.OnFailure != nil {
			w.opts.OnFailure(kind)
		}
		return fmt.Errorf("audit %s: %w", kind, err)
	}
	return nil
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

// clean strips control and format characters, redacts secrets, and caps s at limit bytes,
// ending in "..." when it cut. Redaction runs after the strip, so a token cannot hide
// behind an invisible character, and before the cut, so a token cannot be cut short of
// the pattern.
func clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = store.RedactSecrets(s)
	if len(s) <= limit {
		return s
	}
	cut := limit - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
