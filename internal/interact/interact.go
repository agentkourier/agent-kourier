// Package interact turns an agent's pause into a question in the thread (spec 8.4). The intake
// records a pause as an interaction and posts its prompt through the outbox, and when the same pause
// comes again after a crash it finishes what the crash left undone.
package interact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Binding is what the intake needs of a thread's Binding: whether it allows questions, whether it allows tool
// approvals, and how long a pause waits. The two flags are independent: AskUser gates only questions and ToolApprovals
// only approvals. A pause the Binding does not allow is turned away (refuseAskUser, autoDeny).
type Binding struct {
	Dialect  dialect.Dialect
	Identity string // the Binding's service identity, for the audit rows
	AskUser  bool
	// ToolApprovals makes the intake post an approval as a prompt and leave it pending, for a press to decide. With it
	// false, an approval is denied through the dialect as soon as it arrives (autoDeny). A press on a prompt that was posted
	// before it was turned off decides nothing. When that pause comes again, a pending approval is denied by policy; a human
	// answer already recorded is resent without a policy-denial notice. A prompt permanently abandoned by the outbox is
	// refused as unusable, audited and closed immediately.
	ToolApprovals bool
	// ApproverGroups are the platform user groups whose members decide a tool approval: a channel member who is in none of
	// them is refused (a press, never an ask-user answer). Empty means any non-bot member of the channel.
	ApproverGroups []string
	Timeout        time.Duration
}

// Lookup returns the Binding of a name; the broker implements it (WP-35h).
type Lookup interface {
	Binding(name string) (Binding, error)
}

// Fitter says whether a prompt can be shown in one message: chat.Platform. It is pure, and is asked before a
// prompt is posted, so that a pause that cannot be shown is refused as unusable, which no retry mends.
type Fitter interface {
	Fits(p chat.Prompt) error
}

// Sender is *outbox.Outbox: Send posts at once, Enqueue only writes the row for the drain to post.
type Sender interface {
	Send(ctx context.Context, p outbox.Post) (chat.Ref, error)
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
}

// Config configures an Intake. Store, AuditReader, Lookup, Platform, Outbox, Session and Audit are required.
type Config struct {
	Store store.Interactions
	// AuditReader is the durable audit log used to prove and repair recovery actions.
	AuditReader store.Audit
	Lookup      Lookup
	Platform    Fitter
	Outbox      Sender
	Session     Session
	Audit       Auditor
	// OnApprovalOutcome is called once for each pause a Binding's policy turned away, for the
	// approval-outcome metric (WP-35f2). The outcomes are a closed set. Required.
	OnApprovalOutcome func(binding string, outcome Outcome)
	Now               func() time.Time // time.Now if nil
	Logger            *slog.Logger     // slog.Default() if nil
}

// Intake records pauses as interactions and posts their prompts. It is safe for concurrent use.
type Intake struct {
	cfg         Config
	auditReader store.Audit // the durable log proves and repairs recovery actions
	auditMu     sync.Mutex  // serializes an audit lookup and any repair append
}

// New returns an Intake over cfg.
func New(cfg Config) (*Intake, error) {
	if cfg.Store == nil || cfg.AuditReader == nil || cfg.Lookup == nil || cfg.Platform == nil || cfg.Outbox == nil || cfg.Session == nil || cfg.Audit == nil {
		return nil, errors.New("interact: Store, AuditReader, Lookup, Platform, Outbox, Session and Audit are required")
	}
	if cfg.OnApprovalOutcome == nil {
		return nil, errors.New("interact: OnApprovalOutcome is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Intake{cfg: cfg, auditReader: cfg.AuditReader}, nil
}

// Pause is a task paused on input, as the turn's FinalEvent carried it; reconciliation re-emits it after a restart.
type Pause struct {
	Key     store.ConversationKey
	Binding string
	Task    a2a.Task
	// Recovered says the task was found again after a restart. A recovered pause may already be past its
	// original interaction deadline, so the tap gives a failed hand-off a fresh retry window.
	Recovered bool
}

// InteractionID is the ID a pause becomes: a hash of its kind, task, request ID and tool IDs, so the
// same pause always has the same ID and agent text never reaches one.
func InteractionID(in dialect.Interaction) string {
	tools := make([]string, len(in.Tools))
	for i, t := range in.Tools {
		tools[i] = t.ID
	}
	slices.Sort(tools)
	var b strings.Builder
	for _, f := range append([]string{string(in.Kind), string(in.TaskID), in.RequestID}, tools...) {
		fmt.Fprintf(&b, "%d:%s;", len(f), f) // length-prefixed, so no two field lists hash alike
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "ix-" + hex.EncodeToString(sum[:])[:32]
}

// Paused takes up a pause. A tool approval on a Binding that does not allow them is denied, and a question on one
// that does not allow questions is cancelled. An approval that the prompt cannot show exactly (exactTools) is refused
// as unusable, and so is a pause whose prompt does not fit in one message. Any other pause is recorded and posted as a
// prompt. Every step is idempotent, so a crash between two leaves the next call to
// finish the work and never repeat it: it creates the interaction, posts the prompt if that is not
// done (the outbox adopts a post made and not recorded), and records the message. It does I/O, so a
// caller on the agent's stream goroutine runs it elsewhere.
func (i *Intake) Paused(ctx context.Context, p Pause) error {
	b, err := i.cfg.Lookup.Binding(p.Binding)
	if err != nil {
		return fmt.Errorf("interact: Binding %q: %w", p.Binding, err)
	}
	in, err := b.Dialect.ParseInteraction(p.Task)
	switch {
	case errors.Is(err, dialect.ErrUnusablePause):
		return i.refuseUnusable(ctx, p, b, err)
	case err != nil:
		return fmt.Errorf("interact: parse the pause of task %s: %w", p.Task.ID, err)
	case in == nil:
		return nil // nothing structured to answer: the thread's own replies carry on
	}
	switch {
	case in.Kind == dialect.ToolApproval && !b.ToolApprovals:
		return i.autoDeny(ctx, p, b, *in)
	case in.Kind != dialect.ToolApproval && !b.AskUser:
		return i.refuseAskUser(ctx, p, b, *in)
	case in.Kind == dialect.ToolApproval:
		// An approval whose prompt would not show what the tool will run is refused before anything is recorded: no
		// interaction is made for it, and the request of one that reads differently each time (exactTools) is never stored.
		if err := exactTools(in.Tools); err != nil {
			return i.refuseUnusable(ctx, p, b, err)
		}
	}
	row, err := i.record(ctx, p, b, *in)
	if errors.Is(err, dialect.ErrUnusablePause) { // a second question under a seen ID
		return i.refuseUnusable(ctx, p, b, err)
	} else if err != nil {
		return err
	}
	switch row.State {
	case store.InteractionPending:
		if row.MessageRef == "" {
			err := i.post(ctx, p, row, *in)
			if errors.Is(err, chat.ErrTooLarge) {
				return i.refuseTooLarge(ctx, p, b, row, err)
			}
			if errors.Is(err, outbox.ErrAbandoned) && in.Kind == dialect.ToolApproval {
				return i.refuseAbandoned(ctx, p, b, row, err)
			}
			return err
		}
	case store.InteractionAnswered: // the pause is back, so the answer did not arrive
		return i.resend(ctx, p, b, row)
	case store.InteractionTimedOut, store.InteractionCancelled: // the cancel, or the denial, may not have been made
		return i.endAgain(ctx, p, b, row)
	}
	return nil
}

// endAgain ends a pause that has come again after its interaction was closed without an answer. A tool approval that timed
// out (the decoded request's Kind, never the row's) under a Binding that allows approvals is denied again, from the stored
// request, as the sweep denied it: the denial may not have reached the agent before a crash, or was dropped because the
// session had not parked on the task yet. Everything else is cancelled again, as it was before approvals could be decided:
// a question that timed out, anything cancelled, an approval under a Binding that does not allow them (the sweep cancelled
// it), and an approval whose request retention has cleared or that the dialect cannot answer. A task that is no longer
// paused is fine. A cancelled row with a durable unusable-pause refusal takes the same cancellation retry path, without a
// timeout outcome.
//
// The timeout decision audit is normally written before the denial reaches the session and before the row becomes timed out.
// Recovery checks that durable audit first, repairing it before the denial only for an older fallback timeout whose
// post-resolution audit failed. The denial is one attempt, as the sweep's is: the session takes it, and a send that then
// fails reopens the pause and tells Reopened, which the Sweeper reads to cancel the task (cancelUndelivered). So a pause
// that comes again after a restart is denied once more and then cancelled if that fails, and is never denied for ever.
func (i *Intake) endAgain(ctx context.Context, p Pause, b Binding, row store.Interaction) error {
	if in, ok := toolApprovalOf(row); ok && row.State == store.InteractionTimedOut && b.ToolApprovals {
		if done, err := i.denyAgain(ctx, p, b, row, in); done || err != nil {
			return err
		}
	}
	if row.State == store.InteractionCancelled {
		if handled, err := i.endUnusable(ctx, p, b, row); handled || err != nil {
			return err
		}
	}
	reason := "the interaction timed out"
	if row.State == store.InteractionCancelled {
		reason = "the interaction was cancelled"
	}
	err := i.cfg.Session.Cancel(ctx, p.Key, a2a.TaskID(row.TaskID), reason)
	if err != nil && !errors.Is(err, session.ErrNotPaused) {
		return fmt.Errorf("interact: cancel the task of %s again: %w", row.ID, err)
	}
	return nil
}

// denyAgain sends the denial of the timed-out approval in again. done is false when none can be sent, because the dialect will
// not build it or the session refuses it as invalid, neither of which a retry would mend: the caller cancels the task.
// A task that is not paused is fine, and any other failure is returned, for the pause to be tried again.
func (i *Intake) denyAgain(ctx context.Context, p Pause, b Binding, row store.Interaction, in dialect.Interaction) (done bool, err error) {
	if err := i.ensureTimeoutAudit(ctx, p.Binding, b.Identity, row); err != nil {
		return false, err
	}
	msg, err := AnswerMessage(b.Dialect, row.ID, in, denyAll(in, timeoutDenyReason), autoDecider)
	if err != nil {
		i.cfg.Logger.WarnContext(ctx, "interact: the dialect would not build the denial of a timed-out approval, so its task is cancelled", "interaction", row.ID, "err", err)
		return false, nil
	}
	switch err := i.cfg.Session.Answer(ctx, p.Key, &msg); {
	case err == nil:
		return true, nil
	case errors.Is(err, session.ErrNotPaused):
		i.cfg.Logger.InfoContext(ctx, "interact: dropped the denial of a timed-out approval whose task has moved on", "interaction", row.ID, "task", row.TaskID)
		return true, nil
	case errors.Is(err, store.ErrInvalid):
		i.cfg.Logger.WarnContext(ctx, "interact: the session refused the denial of a timed-out approval as invalid, so its task is cancelled", "interaction", row.ID, "err", err)
		return false, nil
	default:
		return false, fmt.Errorf("interact: deny %s again: %w", row.ID, err)
	}
}

// ensureTimeoutAudit serializes the durable lookup and repair so concurrent recovery taps cannot both append the missing
// timeout decision. A broker store exposes the audit reader through store.Store. A narrow interactions-only wrapper cannot
// prove that a timed-out row was audited, so recovery fails closed before sending its denial.
func (i *Intake) ensureTimeoutAudit(ctx context.Context, binding, identity string, row store.Interaction) error {
	i.auditMu.Lock()
	defer i.auditMu.Unlock()
	audited, err := i.timeoutAudited(ctx, row)
	if err != nil {
		return fmt.Errorf("interact: check the timeout audit of %s: %w", row.ID, err)
	}
	if audited {
		return nil
	}
	actor := audit.Actor{Binding: binding, Identity: identity}
	decision := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionTimedOut}
	if err := i.cfg.Audit.Decision(ctx, actor, decision); err != nil {
		return fmt.Errorf("interact: audit the timeout of %s: %w", row.ID, err)
	}
	return nil
}

// timeoutAudited reports whether the durable timeout decision exists. A normal enabled-approval timeout writes it before
// the row is resolved. The fallback path for a Binding that does not allow approvals writes it after resolution, so a
// failed post-resolution audit can leave a timed-out row without one; recovery repairs that gap before sending a denial.
// New requires the reader explicitly because recovery must not silently treat an unavailable audit log as proof.
func (i *Intake) timeoutAudited(ctx context.Context, row store.Interaction) (bool, error) {
	audited := false
	err := i.forEachAudit(ctx, row.CreatedAt, func(entry store.AuditEntry) bool {
		if entry.Kind == store.AuditInteractionDecision && strings.HasPrefix(entry.Subject, "interaction="+row.ID+" task=") && entry.Outcome == string(audit.DecisionTimedOut) {
			audited = true
			return true
		}
		return false
	})
	return audited, err
}

// forEachAudit reads the durable audit log in sequence order. Returning true from visit stops the scan.
func (i *Intake) forEachAudit(ctx context.Context, since time.Time, visit func(store.AuditEntry) bool) error {
	var after int64
	for {
		entries, err := i.auditReader.ListAudit(ctx, since, after, 1000)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			if visit(entry) {
				return nil
			}
		}
		next := entries[len(entries)-1].Sequence
		if next <= after {
			return fmt.Errorf("audit page did not advance past sequence %d", after)
		}
		after = next
		if len(entries) < 1000 {
			return nil
		}
	}
}

type unusablePauseAudit struct {
	refused      bool
	cancelOK     bool
	cancelFailed bool
}

// unusablePauseAudit finds the refusal and cancellation records for one task under
// one Binding service actor. The subject and actor fields use audit's canonical
// cleaning and length limits, and a cancelled interaction without this refusal remains generic.
func (i *Intake) unusablePauseAudit(ctx context.Context, actor audit.Actor, taskID a2a.TaskID, contextID string, since time.Time) (unusablePauseAudit, error) {
	var state unusablePauseAudit
	subject := audit.Subject(taskID, contextID, "")
	err := i.forEachAudit(ctx, since, func(entry store.AuditEntry) bool {
		if entry.Kind != store.AuditAgentCall || entry.Subject != subject || !audit.MatchesActor(entry, actor) {
			return false
		}
		switch {
		case strings.HasPrefix(entry.Outcome, "unusable_pause: "):
			state.refused = true
		case entry.Outcome == "unusable_pause_cancel: ok":
			state.cancelOK = true
		case strings.HasPrefix(entry.Outcome, "unusable_pause_cancel: failed"):
			state.cancelFailed = true
		}
		return false
	})
	return state, err
}

// ensureUnusablePause writes the refusal cause once, before a durable close.
func (i *Intake) ensureUnusablePause(ctx context.Context, actor audit.Actor, taskID a2a.TaskID, contextID string, since time.Time, cause error) error {
	i.auditMu.Lock()
	defer i.auditMu.Unlock()
	state, err := i.unusablePauseAudit(ctx, actor, taskID, contextID, since)
	if err != nil {
		return err
	}
	if state.refused {
		return nil
	}
	return i.cfg.Audit.UnusablePause(ctx, actor, taskID, contextID, cause)
}

// recordUnusablePauseCancel records a known cancellation result. Failed attempts
// are deduplicated; callers suppress repeated successful results. ErrNotPaused is
// deliberately not passed here because it cannot prove that this call cancelled the task.
func (i *Intake) recordUnusablePauseCancel(ctx context.Context, actor audit.Actor, taskID a2a.TaskID, contextID string, since time.Time, cancelErr error) error {
	i.auditMu.Lock()
	defer i.auditMu.Unlock()

	// A nil error is the result of this call's Cancel, so its audit is known without a
	// post-cancel read. The task may already be gone on a later retry, but that retry
	// returns ErrNotPaused and never reaches this path.
	if cancelErr == nil {
		return i.cfg.Audit.UnusablePauseCancel(ctx, actor, taskID, contextID, nil)
	}

	state, err := i.unusablePauseAudit(ctx, actor, taskID, contextID, since)
	if err != nil {
		return err
	}
	if state.cancelOK || state.cancelFailed {
		return nil
	}
	return i.cfg.Audit.UnusablePauseCancel(ctx, actor, taskID, contextID, cancelErr)
}

// pausedAt is when the pause began: the task's own timestamp, or now if that is earlier or there is none.
func pausedAt(task a2a.Task, now time.Time) time.Time {
	if ts := task.Status.Timestamp; ts != nil && ts.Before(now) {
		return *ts
	}
	return now
}

// record creates the interaction for in, or finds the one that exists. The deadline is the task's own
// timestamp, or Agent Kourier's clock if that is earlier, plus the Binding's timeout, so a pause seen again
// after a restart keeps its deadline and kagent's clock cannot push it out (the clock is UNCONFIRMED(WP-02)).
//
// The ID names a pause by its task and request ID, which is UNCONFIRMED(WP-02) to be unique: kagent's
// ID is the model's tool call ID, which a model may repeat. So the stored request must be the one just
// parsed; if it is not, this is another question under a seen ID, and it is refused as unusable. (A row that
// retention has cleared is the exception: see below.)
func (i *Intake) record(ctx context.Context, p Pause, b Binding, in dialect.Interaction) (store.Interaction, error) {
	if b.Timeout <= 0 { // config forbids it, but a Lookup is another's code, and a zero would expire every submit
		return store.Interaction{}, fmt.Errorf("interact: Binding %q has no interaction timeout", p.Binding)
	}
	// What is stored has the agent's secrets taken out (storable); the answer is built from the IDs and choices,
	// which that leaves as they were.
	request, err := json.Marshal(storable(in))
	if err != nil {
		return store.Interaction{}, fmt.Errorf("interact: encode the pause: %w", err)
	}
	at := pausedAt(p.Task, i.cfg.Now())
	kind := store.InteractionAskUser
	switch {
	case in.Nested != nil:
		kind = store.InteractionNested
	case in.Kind == dialect.ToolApproval:
		kind = store.InteractionToolApproval
	}
	id := InteractionID(in)
	row := store.Interaction{TextReplies: in.TextReplies, ID: id, Key: p.Key, TaskID: string(in.TaskID), Kind: kind, Request: request, CreatedAt: at, Deadline: at.Add(b.Timeout)}
	if _, err := i.cfg.Store.CreateInteraction(ctx, row); err != nil {
		return store.Interaction{}, fmt.Errorf("interact: record %s: %w", id, err)
	}
	if row, err = i.cfg.Store.GetInteraction(ctx, id); err != nil {
		return store.Interaction{}, fmt.Errorf("interact: read %s: %w", id, err)
	}
	// A settled row whose request retention has cleared cannot be compared, and needs no comparison: the ID names
	// the pause (its task, request and tool IDs), and what is done with a settled row (cancel the task again, or
	// resend the answer) reads nothing of the request. Taking it for a different pause would blame the agent.
	if (row.State == store.InteractionPending || len(row.Request) > 0) && !samePause(row.Request, request) {
		return store.Interaction{}, fmt.Errorf("%w: a second, different pause under the ID of %s", dialect.ErrUnusablePause, id)
	}
	return row, nil
}

// pauseKey is what an answer depends on in a pause: what it asks and of which task. The agent's hint
// and the context are not in it, and neither is anything a later version adds to dialect.Interaction.
type pauseKey struct {
	Kind      dialect.Kind
	TaskID    a2a.TaskID
	RequestID string
	Questions []dialect.Question
	Tools     []toolKey
	Nested    *dialect.Nested
}

type toolKey struct {
	ID, Name string
	Args     map[string]any
}

// samePause reports whether the stored request and the one just made (as it is stored: storable) are the same
// pause, by their decoded fields and not their bytes, so that a row written by an older version, with another
// hint or a field since added, is still the pause it was. A row an older version wrote before secrets were taken
// out is the same pause too, once they are taken out of it: that is for a question. No tagged release wrote an approval row
// before secrets were taken out (a build of main between 2026-10-01 and 2026-10-05 may have, and its row is refused as
// unusable), and for stored text over 4 KiB the second reading lets a pause that was constructed match a row whose stored
// text was never checked, so for a tool approval only the first holds: the stored request is the fresh one's stored form. A request that cannot be read, such as one that has been cleared, is not the same.
func samePause(stored, fresh []byte) bool {
	var a, b dialect.Interaction
	if json.Unmarshal(stored, &a) != nil || json.Unmarshal(fresh, &b) != nil {
		return false
	}
	want := keyOf(b)
	if reflect.DeepEqual(keyOf(a), want) {
		return true
	}
	if b.Kind == dialect.ToolApproval { // a match needs the same kind, so the fresh pause's decides for both
		return false
	}
	return reflect.DeepEqual(keyOf(storable(a)), want)
}

func keyOf(in dialect.Interaction) pauseKey {
	k := pauseKey{Kind: in.Kind, TaskID: in.TaskID, RequestID: in.RequestID, Questions: in.Questions, Nested: in.Nested}
	for _, t := range in.Tools {
		k.Tools = append(k.Tools, toolKey{t.ID, t.Name, t.Args})
	}
	// In the order of their IDs, as InteractionID takes them: the same tools listed in another order
	// are the same pause, and must not be refused as a different one under the same ID.
	slices.SortFunc(k.Tools, func(a, b toolKey) int { return strings.Compare(a.ID, b.ID) })
	return k
}

// post sends the prompt through the outbox, role reply, source the interaction ID (spec 8.6 step 5). A prompt
// the platform cannot show in one message is an error wrapping chat.ErrTooLarge, before anything is sent.
//
// A question's prompt is made from the pause as parsed (promptOf). An approval's is made from the stored request
// (row.Request) and not from the pause just parsed, with toolsOf and nothing else: the same tools in another order are the
// same pause (keyOf), and a Submit's radios are positional, so what is shown, what a press indexes and what the ending shows
// must be one order, the stored one. The stored request is already redacted, and it is neither checked again nor redacted
// again: redaction lengthens text, so the stored form of a pause that was accepted can be longer than the bound that
// exactTools holds a raw one to, and checking it again would refuse a pause that was accepted. The exactness of what is
// shown is checked where the raw pause is: Paused calls exactTools on it before it records anything, and post is called
// from nowhere else; promptOf, which checks and redacts, is the way to the prompt of a raw approval. Whether the stored
// prompt fits the platform is Fits's to say, below, and a prompt that does not is refused as too large by the caller.
func (i *Intake) post(ctx context.Context, p Pause, row store.Interaction, in dialect.Interaction) error {
	var prompt chat.Prompt
	var err error
	if in.Kind == dialect.ToolApproval {
		var stored dialect.Interaction
		if err := json.Unmarshal(row.Request, &stored); err != nil {
			return fmt.Errorf("interact: the stored request of %s: %w", row.ID, err)
		}
		prompt, err = toolsOf(row.ID, stored, row.Deadline)
	} else {
		prompt, err = promptOf(row.ID, in, row.Deadline)
	}
	if err != nil {
		return fmt.Errorf("interact: make the prompt of %s: %w", row.ID, err)
	}
	if err := i.cfg.Platform.Fits(prompt); err != nil {
		return fmt.Errorf("%w (the prompt of %s)", err, row.ID) // the reason first: an audit row keeps the start of it
	}
	ref, err := i.cfg.Outbox.Send(ctx, outbox.Post{Source: row.ID, Role: store.OutboxReply, Channel: p.Key.Channel, Thread: p.Key.Thread, Out: chat.Ask(prompt)})
	if err != nil {
		return fmt.Errorf("interact: post the prompt of %s: %w", row.ID, err)
	}
	if err := i.cfg.Store.SetInteractionMessageRef(ctx, row.ID, ref.ID); err != nil {
		return fmt.Errorf("interact: record the prompt of %s: %w", row.ID, err)
	}
	return nil
}
