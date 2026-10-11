package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Updater edits a message: chat.Platform.
type Updater interface {
	Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error
	// RefAt is the Ref of the message recorded by its ID alone, as an interaction records its prompt.
	RefAt(channel, thread, id string) chat.Ref
}

// Enqueuer writes a post for the outbox to deliver: *outbox.Outbox.
type Enqueuer interface {
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
}

// SessionReader reads a conversation's session, to name its Binding: the session store.
type SessionReader interface {
	GetSession(ctx context.Context, key store.ConversationKey, ttl time.Duration, now time.Time) (store.Session, error)
}

// PressQueue knows the presses that are recorded and not yet decided (WP-33d1 implements it).
type PressQueue interface {
	// PendingPresses reports whether the interaction has a press, recorded at or before the time
	// given, that has not been decided.
	PendingPresses(ctx context.Context, interactionID string, before time.Time) (bool, error)
}

// The notices of a timeout are fixed text. They name no agent text, and say no more than was done: not that the task was
// cancelled, which it may not have been, and not that a denial reached the agent, which nothing here can know.
//
// A question's is timedOutNotice, and so is the notice of an approval on a Binding that does not allow approvals, or whose
// Binding is not in the config: for those the sweep does what it did before approvals could be decided, and says what it said.
// The others are an approval's on a Binding that allows them, by what the sweep did: the session took the denial; the task was
// not paused, so nothing was sent; the denial could not be made and the task was cancelled; and it was cancelled when it was
// not paused, so that nothing could be done.
const (
	timedOutNotice        = "No one answered this question in time, so it was closed."
	timedOutDeniedNotice  = "No one decided this request in time, so Agent Kourier is denying it."
	timedOutClosedNotice  = "No one decided this request in time, so it was closed."
	timedOutCancelNotice  = "No one decided this request in time, so Agent Kourier is cancelling the task."
	timedOutNothingNotice = "No one decided this request in time, and Agent Kourier could not act on it."
)

// timeoutDenyReason is the reason the denial of a timed-out approval carries to the agent. It is its own, apart from
// denyReason, so that the agent's log tells a request nobody decided from one the Binding does not allow.
const timeoutDenyReason = "denied by agent-kourier: no one decided in time"

// maxAttempts is how many sweeps may fail at the edit or the notice of a timeout before the step is
// given up on, so that one prompt that cannot be edited does not hold its thread. The cancel is
// never given up on: a task that is still paused holds its thread and the replies queued behind it,
// and only this sweep would end it.
const maxAttempts = 5

// readAll is a thread TTL no session outlives, to read a session whatever its age.
const readAll = 100 * 365 * 24 * time.Hour

// SweepConfig configures a Sweeper. Everything but Now, Logger, Interval, Batch and Grace is required.
type SweepConfig struct {
	Store    store.Interactions
	Sessions SessionReader
	// Lookup gives the Binding of an approval's thread, whose dialect builds the denial that ends the approval.
	Lookup  Lookup
	Session Session
	Updater Updater
	Outbox  Enqueuer
	Auditor Auditor
	// Presses is asked before an interaction is timed out: one with a press recorded by its
	// deadline and not yet decided is left, because deciding a press can take longer than the
	// grace, and cancelling the task under it would drop an answer that came in time.
	Presses PressQueue
	Now     func() time.Time // time.Now if nil
	Logger  *slog.Logger     // slog.Default() if nil
	// Interval is how often Run sweeps (default 10 seconds).
	Interval time.Duration
	// Batch is how many interactions one sweep times out at most (default 10), so that a backlog
	// after an outage reaches Slack a few edits at a time.
	Batch int
	// Grace is how long past its deadline an interaction waits before it is swept (default 5
	// seconds), so that a press that was being decided at the deadline has been recorded first.
	Grace time.Duration
	// OnTimeout is called once for each interaction that timed out, with its ID, for a metric. Required.
	OnTimeout func(id string)
	// OnApprovalOutcome is called once for each tool approval on a Binding that allows them (Binding.ToolApprovals) that
	// timed out, with the Binding's name and OutcomeTimedOut, in the sweep that records the timeout. A question never calls
	// it, nor does an approval on any other Binding, or on one that is not in the config: those are ended as a question's is.
	// Required.
	OnApprovalOutcome func(binding string, outcome Outcome)
}

// Sweeper times out the interactions nobody answered (spec 8.4 step 6).
//
// UNCONFIRMED: a task that resumes outside Agent Kourier (from kagent's own UI, say) and pauses again is a
// new pause, and becomes a new interaction while the old one is still pending. The old one is swept
// when it is due, and its cancel, or its denial, names the old pause's task, which is the task that is paused now. Every step
// before the timeout is recorded can be repeated (a denial the session took is not sent again by this process: see
// denyApproval), so a sweep that dies halfway is finished by the next, and only the sweep that records the timeout audits and
// counts it.
type Sweeper struct {
	cfg SweepConfig
	mu  sync.Mutex     // one sweep at a time
	try map[string]int // sweeps that have failed at a step, by interaction; guarded by mu
	// taken is the end of each approval whose denial the session took in this process and whose timeout is not yet
	// recorded, by interaction; guarded by mu. A later sweep of it does not send the denial again (see denyApproval).
	taken map[string]pauseEnd
}

// NewSweeper returns a Sweeper over cfg.
func NewSweeper(cfg SweepConfig) (*Sweeper, error) {
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Lookup == nil || cfg.Session == nil || cfg.Updater == nil || cfg.Outbox == nil || cfg.Auditor == nil || cfg.Presses == nil {
		return nil, errors.New("interact: Store, Sessions, Lookup, Session, Updater, Outbox, Auditor and Presses are required")
	}
	if cfg.OnTimeout == nil {
		return nil, errors.New("interact: OnTimeout is required")
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
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 10
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 5 * time.Second
	}
	return &Sweeper{cfg: cfg, try: map[string]int{}, taken: map[string]pauseEnd{}}, nil
}

// Run sweeps at once and then every Interval until ctx ends, and returns its error.
func (s *Sweeper) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.WarnContext(ctx, "interact: some interactions were not timed out; they will be tried again", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep times out up to Batch of the interactions that are past their deadline and grace, oldest
// first, except those with a press waiting to be decided (see Presses). For each it ends the pause (below), replaces the
// prompt, enqueues the notice, records the timeout, and then audits and counts it. A step that fails leaves the interaction
// pending for the next sweep. It goes on past one it cannot finish, and returns every such error joined.
//
// It lists only pending rows, and an answer is recorded only on a pending row before its deadline (SubmitInteraction), so no
// person's answer can exist for a row when it is listed. One recorded in between wins at the record of the timeout
// (ResolveInteraction answers ErrConflict, and nothing is audited or counted); by then the pause was ended, the prompt was
// edited to timed out and the notice enqueued, though the record says the person decided. A press recorded by the deadline
// holds the sweep off (Presses), which is what makes that window as short as the grace.
//
// A question's pause is ended by cancelling its task. A tool approval's (the decoded request's Kind, never the row's: a
// sub-agent's is stored as nested) is ended by denying every tool through the Binding's dialect (spec 8.4 step 6) only on a
// Binding that allows approvals (Binding.ToolApprovals); on any other, or when the Binding is not in the config, it is ended
// as a question's is, with a question's notice, and is not counted as an approval's outcome.
func (s *Sweeper) Sweep(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	due, err := s.cfg.Store.ListDueInteractions(ctx, now.Add(-s.cfg.Grace))
	if err != nil {
		return fmt.Errorf("interact: list the interactions that are due: %w", err)
	}
	var errs []error
	done := 0
	for _, row := range due {
		if done == s.cfg.Batch {
			break
		}
		switch waiting, err := s.cfg.Presses.PendingPresses(ctx, row.ID, row.Deadline); { // if it cannot be known, wait
		case err != nil:
			errs = append(errs, fmt.Errorf("interact: presses of %s: %w", row.ID, err))
			continue
		case waiting:
			continue // not counted against the batch: it is not work
		}
		done++
		if err := s.timeOut(ctx, row, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pauseEnd is how the pause of an interaction was ended, which says what its thread is told and whether it is counted as the
// timeout of a tool approval.
type pauseEnd struct {
	notice string
	// binding is the name of the Binding the approval's thread is under, and counted is whether the timeout is counted as an
	// approval's outcome: only for an approval on a Binding that allows them, whose name is known.
	binding string
	counted bool
}

// timeOut does the steps for one interaction, then records the timeout.
func (s *Sweeper) timeOut(ctx context.Context, row store.Interaction, now time.Time) error {
	s.try[row.ID]++
	final := s.try[row.ID] >= maxAttempts // the sweep on which the edit and the notice are given up on
	in, approval := toolApprovalOf(row)
	end := pauseEnd{notice: timedOutNotice}
	endName, endPause := "cancel the task", func() error { return s.cancel(ctx, row) }
	if approval {
		endName, endPause = "end the pause of the approval", func() error {
			e, err := s.endApproval(ctx, row, in, now)
			if err == nil {
				end = e
			}
			return err
		}
	}
	// The order matters: the pause is ended first (the task is cancelled, or an approval's tools are denied), so that
	// nothing is said or edited for a task that is still paused, and the timeout recorded last, so that a sweep that dies
	// on the way is finished by the next.
	for _, step := range []struct {
		name      string
		essential bool // never given up on
		edit      bool // giving it up leaves the prompt with its controls
		do        func() error
	}{
		{endName, true, false, endPause},
		{"replace the prompt", false, true, func() error { return s.replace(ctx, row) }},
		{"enqueue the notice", false, false, func() error { return s.notify(ctx, row, end.notice) }},
	} {
		err := step.do()
		switch {
		case err == nil:
		case step.essential || !final:
			return fmt.Errorf("interact: time out %s: %s: %w", row.ID, step.name, err)
		case step.edit:
			// The prompt keeps its buttons although the question timed out: a press on one is refused,
			// but nothing yet edits it again. WP-33d3 repairs it, and finds it by this line.
			s.cfg.Logger.WarnContext(ctx, "interact: the prompt of a timed-out interaction keeps its controls: the edit was given up on", "interaction", row.ID, "err", err)
		default:
			s.cfg.Logger.ErrorContext(ctx, "interact: giving up on a step of a timeout", "interaction", row.ID, "step", step.name, "err", err)
		}
	}
	err := s.cfg.Store.ResolveInteraction(ctx, row.ID, store.InteractionTimedOut, now)
	switch {
	case errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound): // decided in the meantime
		s.forget(row.ID)
		return nil
	case err != nil:
		return fmt.Errorf("interact: record the timeout of %s: %w", row.ID, err)
	}
	s.forget(row.ID)
	s.report(ctx, row, end, now)
	return nil
}

// forget drops what the sweeper remembers of an interaction whose timeout is recorded or was decided otherwise.
func (s *Sweeper) forget(id string) {
	delete(s.try, id)
	delete(s.taken, id)
}

// toolApprovalOf decodes the stored request of row and reports whether it is a tool approval. The request's Kind says
// so and the row's does not: a sub-agent's approval is stored with the kind nested. A request that cannot be read, or
// has been cleared, is not one, and is ended as a question is.
func toolApprovalOf(row store.Interaction) (dialect.Interaction, bool) {
	var in dialect.Interaction
	if len(row.Request) == 0 || json.Unmarshal(row.Request, &in) != nil || in.Kind != dialect.ToolApproval {
		return dialect.Interaction{}, false
	}
	return in, true
}

// cancel cancels the task of the question. It names that task, so a newer one the thread has moved
// on to is not touched: the session answers ErrNotPaused for one that is not the paused task.
func (s *Sweeper) cancel(ctx context.Context, row store.Interaction) error {
	_, err := s.cancelled(ctx, row)
	return err
}

// cancelled is cancel, and says whether the session cancelled the task (false: it was not paused).
func (s *Sweeper) cancelled(ctx context.Context, row store.Interaction) (bool, error) {
	err := s.cfg.Session.Cancel(ctx, row.Key, a2a.TaskID(row.TaskID), "the interaction timed out")
	switch {
	case errors.Is(err, session.ErrNotPaused):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// endApproval ends the pause of the tool approval in.
//
// Only a Binding that allows approvals (Binding.ToolApprovals) has its approval denied. For any other, and for one whose
// Binding is not in the config or whose thread has no session to name it, the task is cancelled as a question's is, with a
// question's notice and no count; that is what the sweep did before approvals could be decided, and it is logged when the
// Binding was expected. A session that cannot be read says nothing of the Binding, and is an error, to be tried again.
func (s *Sweeper) endApproval(ctx context.Context, row store.Interaction, in dialect.Interaction, now time.Time) (pauseEnd, error) {
	asked := pauseEnd{notice: timedOutNotice}
	sess, err := s.cfg.Sessions.GetSession(ctx, row.Key, readAll, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.cfg.Logger.WarnContext(ctx, "interact: a timed-out approval has no session to name its Binding, so its task is cancelled", "interaction", row.ID)
		return asked, s.cancel(ctx, row)
	case err != nil:
		return asked, fmt.Errorf("read the session: %w", err)
	}
	b, err := s.cfg.Lookup.Binding(sess.Binding)
	switch {
	case err != nil:
		s.cfg.Logger.WarnContext(ctx, "interact: the Binding of a timed-out approval is not available, so its task is cancelled", "interaction", row.ID, "binding", sess.Binding, "err", err)
		return asked, s.cancel(ctx, row)
	case !b.ToolApprovals:
		return asked, s.cancel(ctx, row)
	}
	return s.denyApproval(ctx, row, in, b, sess.Binding)
}

// denyApproval sends the denial of every tool of the approval in, under the Binding b (named binding), with Agent Kourier
// as the attribution and the message ID the interaction's answer would have (AnswerMessage). It names the approval's task,
// so a newer one the thread has moved on to is not touched. The denial is rebuilt from the stored request, and is not
// stored as the row's answer, which is a person's.
//
// What Session.Answer says is all the sweep learns. nil: the session took the denial; it will send it, which it does after
// this returns, and whether the agent received it the sweep never learns (a send that fails after the session took it
// reopens the pause without telling this package: UNCONFIRMED, known limit; nothing here retries it, and the task may stay
// paused behind the closed row until a restart, where the pause comes again and Intake.Paused sends the denial again).
// ErrNotPaused: nothing was sent, as the task was not paused on that task, or the session has not parked on it yet. ErrInvalid
// is final, and a request the dialect will not answer is too, and no retry would mend either: the task is cancelled instead,
// as a question's is. Any other error (the session not ready yet, say) is returned, and the denial is tried again; the
// essential step is never given up on.
//
// A denial the session took is remembered, and a later sweep of the same row (a later step failed) does not send it again:
// sent again, it could meet a newer pause of the same task, which the session matches by task ID alone, and kagent answers
// tool IDs it does not know by wedging the task. UNCONFIRMED: a restart between the denial and the record, a kagent that
// pauses the same task twice, and what it does with a repeated message ID.
func (s *Sweeper) denyApproval(ctx context.Context, row store.Interaction, in dialect.Interaction, b Binding, binding string) (pauseEnd, error) {
	if end, ok := s.taken[row.ID]; ok {
		return end, nil
	}
	denied := pauseEnd{notice: timedOutDeniedNotice, binding: binding, counted: true}
	cancelled := func() (pauseEnd, error) { // the fallback
		end := pauseEnd{notice: timedOutCancelNotice, binding: binding, counted: true}
		took, err := s.cancelled(ctx, row)
		if err == nil && !took {
			end.notice = timedOutNothingNotice
		}
		return end, err
	}
	msg, err := AnswerMessage(b.Dialect, row.ID, in, denyAll(in, timeoutDenyReason), autoDecider)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "interact: the dialect would not build the denial of a timed-out approval, so its task is cancelled", "interaction", row.ID, "err", err)
		return cancelled()
	}
	switch err := s.cfg.Session.Answer(ctx, row.Key, &msg); {
	case err == nil:
		s.taken[row.ID] = denied
		return denied, nil
	case errors.Is(err, session.ErrNotPaused):
		return pauseEnd{notice: timedOutClosedNotice, binding: binding, counted: true}, nil
	case errors.Is(err, store.ErrInvalid):
		s.cfg.Logger.WarnContext(ctx, "interact: the session refused the denial of a timed-out approval as invalid, so its task is cancelled", "interaction", row.ID, "err", err)
		return cancelled()
	default:
		return pauseEnd{}, err
	}
}

// replace edits the prompt, if it was posted, into the timed-out message.
func (s *Sweeper) replace(ctx context.Context, row store.Interaction) error {
	if row.MessageRef == "" {
		return nil
	}
	var in dialect.Interaction // zero if the stored request cannot be read: the ending still says that it timed out
	if err := json.Unmarshal(row.Request, &in); err != nil {
		s.cfg.Logger.WarnContext(ctx, "interact: the stored request cannot be read", "interaction", row.ID)
	}
	e, err := endingOf(chat.TimedOut, "", row.Deadline, in, nil)
	if err != nil {
		return fmt.Errorf("interact: the ending of %s: %w", row.ID, err)
	}
	return s.cfg.Updater.Update(ctx, s.cfg.Updater.RefAt(row.Key.Channel, row.Key.Thread, row.MessageRef), chat.End(e))
}

// notify enqueues the notice, text. Its ID comes from the interaction, so the same one is never posted twice.
func (s *Sweeper) notify(ctx context.Context, row store.Interaction, text string) error {
	_, err := s.cfg.Outbox.Enqueue(ctx, outbox.Post{Source: row.ID, Role: store.OutboxNotice, Channel: row.Key.Channel,
		Thread: row.Key.Thread, Out: chat.Say(render.Notice(text, ""))})
	return err
}

// report audits a timeout this sweep recorded and counts it: OnTimeout for any interaction, and OnApprovalOutcome for the
// tool approval of a Binding that allows them, under the Binding's name that the sweep found in ending the pause (so that a
// failed read of the session now does not leave the count without it). The timeout is done, so a failure to audit is logged
// and never undoes it (spec 8.6 step 7); the writer meters the failure itself.
func (s *Sweeper) report(ctx context.Context, row store.Interaction, end pauseEnd, now time.Time) {
	var actor audit.Actor // no person decided it
	if sess, err := s.cfg.Sessions.GetSession(ctx, row.Key, readAll, now); err == nil {
		actor.Binding = sess.Binding
	} else {
		actor.Binding = end.binding
	}
	d := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionTimedOut, At: now}
	if err := s.cfg.Auditor.Decision(ctx, actor, d); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "interact: a timeout was not audited", "interaction", row.ID, "err", err)
	}
	if end.counted {
		s.cfg.OnApprovalOutcome(end.binding, OutcomeTimedOut)
	}
	s.cfg.OnTimeout(row.ID)
}
