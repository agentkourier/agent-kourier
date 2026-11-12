package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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

// Abandoner ends the pause of a task without asking the agent, for a pause whose Binding the config has not got: *session.Manager.
type Abandoner interface {
	// Abandon releases the runner parked on taskID and clears the task from the conversation's session, calling nothing. It answers
	// session.ErrNotPaused when the conversation is not paused on that task.
	Abandon(ctx context.Context, key store.ConversationKey, taskID a2a.TaskID, reason string) error
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
// not paused, so that nothing could be done. A denial that the session took and the agent did not is told afterwards, by its
// own notice (undeliveredDenialNotice).
const (
	timedOutNotice        = "No one answered this question in time, so it was closed."
	timedOutDeniedNotice  = "No one decided this request in time, so Agent Kourier is denying it."
	timedOutClosedNotice  = "No one decided this request in time, so it was closed."
	timedOutCancelNotice  = "No one decided this request in time, so Agent Kourier is cancelling the task."
	timedOutNothingNotice = "No one decided this request in time, and Agent Kourier could not act on it."
	// undeliveredDenialNotice follows timedOutDeniedNotice when the session took the denial and the agent did not (see
	// Reopened): the task still waited for input, and Agent Kourier cancels it. It is the thread's only word of that, so it says
	// the cancel is under way and not that it is done, which it may not be.
	undeliveredDenialNotice = "The agent did not take Agent Kourier's denial of this request, so Agent Kourier is cancelling the task."
	// undeliveredLeftNotice replaces it when the Binding is gone for longer than the OrphanGrace: there is nothing to cancel the task through, so
	// the pause is released and the task is left with the agent.
	undeliveredLeftNotice = "The agent did not take Agent Kourier's denial of this request, and Agent Kourier can no longer reach the agent to cancel the task, so the task is left with the agent."
)

// timeoutDenyReason is the reason the denial of a timed-out approval carries to the agent. It is its own, apart from
// denyReason, so that the agent's log tells a request nobody decided from one the Binding does not allow.
const timeoutDenyReason = "denied by agent-kourier: no one decided in time"

// maxAttempts is how many sweeps may fail at the edit or the notice of a timeout before the step is
// given up on, so that one prompt that cannot be edited does not hold its thread. The cancel is
// never given up on: a task that is still paused holds its thread and the replies queued behind it,
// and only this sweep would end it.
const maxAttempts = 5

// defaultOrphanGrace is how long a due interaction must be seen, at every sweep, without its Binding in the config or with its chat
// connection not served, before the sweep ends it without the agent (SweepConfig.OrphanGrace). The config source defers the deletion
// of an object by up to a minute and a read of the cluster is every 10 seconds, the supervisor looks at the connections every 5
// seconds, and a takeover or a rollback of the chart takes the objects away one after another: five minutes is several of each, and short
// against the pauses it ends, which have waited for their whole timeout already.
const defaultOrphanGrace = 5 * time.Minute

// SweepConfig configures a Sweeper. Everything but Served, Now, Logger, Interval, Batch, Grace and OrphanGrace is required.
type SweepConfig struct {
	Store    store.Interactions
	Sessions SessionReader
	// Lookup gives the Binding of an interaction's thread: its dialect builds the denial that ends an approval, and a Binding it does not
	// have (an error wrapping session.ErrUnknownBinding) is what makes the interaction an orphan's, ended without the agent.
	Lookup  Lookup
	Session Session
	// Abandoner ends the pause of an orphan, whose Binding is not in the config, so that Session.Cancel could only fail.
	Abandoner Abandoner
	// Updaters gives the platform of an interaction's connection, which edits its prompt.
	Updaters chat.Resolver[Updater]
	Outbox   Enqueuer
	Auditor  Auditor
	// Presses is asked before an interaction is timed out: one with a press recorded by its
	// deadline and not yet decided is left, because deciding a press can take longer than the
	// grace, and cancelling the task under it would drop an answer that came in time.
	Presses PressQueue
	// Served says whether this broker serves the chat connection named conn ("namespace/name"): the config has it and it is not refused (a second
	// connection of a Slack app another connection uses). A connection that is served and not running for now (it has not started, or its
	// credentials were rejected: Updaters answers that with chat.ErrConnectionDown) is not the gone one: its interactions wait for it. A connection
	// that is not in the config (deleted, invalid with no good generation, outside the admission list, or past the 16) is not served, however its Binding is, since it cannot run while it stays so. An interaction of a connection that is
	// not served is an orphan's even though its Binding is in the config. Nil: every connection is served.
	Served func(conn string) bool
	// Reopened tells the Sweeper of the denials the session took and then failed to deliver, so that their tasks are cancelled.
	// It is the session's AnswerWatcher (session.Config.AnswerWatcher): the same value. Required.
	Reopened *Reopened
	Now      func() time.Time // time.Now if nil
	Logger   *slog.Logger     // slog.Default() if nil
	// Interval is how often Run sweeps (default 10 seconds).
	Interval time.Duration
	// Batch is how many interactions one sweep times out at most (default 10), so that a backlog
	// after an outage reaches Slack a few edits at a time.
	Batch int
	// Grace is how long past its deadline an interaction waits before it is swept (default 5
	// seconds), so that a press that was being decided at the deadline has been recorded first.
	Grace time.Duration
	// OrphanGrace is how long a due interaction must have been seen without its Binding in the config, or with its chat connection not served
	// (Served), at every sweep that looked at it, before it is ended without the agent (default 5 minutes). A Binding that is back, or
	// a connection that is served again, in the meantime starts the count again.
	OrphanGrace time.Duration
	// OnTimeout is called once for each interaction that timed out, with its ID, for a metric. Required.
	OnTimeout func(id string)
	// OnApprovalOutcome is called once for each tool approval on a Binding that allows them (Binding.ToolApprovals) that
	// timed out, with the Binding's name and OutcomeTimedOut, in the sweep that records the timeout. A question never calls
	// it, nor does an approval on any other Binding: those are ended as a question's is. An interaction of either kind that is ended
	// without the agent (an orphan's) calls it with OutcomeOrphaned instead, and calls neither OnTimeout nor the approval's outcome.
	// Required.
	OnApprovalOutcome func(binding string, outcome Outcome)
}

// Sweeper times out the interactions nobody answered (spec 8.4 step 6).
//
// UNCONFIRMED: a task that resumes outside Agent Kourier (from kagent's own UI, say) and pauses again is a
// new pause, and becomes a new interaction while the old one is still pending. The old one is swept
// when it is due, and its cancel, or its denial, names the old pause's task, which is the task that is paused now. Every step
// before the timeout is recorded can be repeated (a denial the session took is not sent again by this process: see
// denyApproval), so a sweep that dies halfway is finished by the next. A timeout audit for an enabled approval is written
// before its denial and may be written again after a restart while the row is still pending; once the row is durably timed
// out, intake recovery reuses that audit while retrying delivery. The sweep that records the timeout counts it.
type Sweeper struct {
	cfg SweepConfig
	mu  sync.Mutex     // one sweep at a time
	try map[string]int // sweeps that have failed at a step, by interaction; guarded by mu
	// downSaid is the connections whose due interactions were left because they do not run, for which a WARN was logged; guarded by mu.
	downSaid map[string]bool
	// orphanSince is when each due interaction was first seen as an orphan, by interaction, and has been seen as one at every sweep since;
	// guarded by mu. Only a sweep that finds it not an orphan, or the interaction settled, drops it.
	orphanSince map[string]time.Time
	// audited is the timeout decision audit written before a tool-approval denial reaches the
	// session. It is process-local because a restart may write the audit again while the row is
	// still pending; the interaction row remains the durable first-complete decision point.
	audited map[string]timeoutAudit
	// taken is the end of each approval whose denial the session took in this process and whose timeout is not yet
	// recorded, by interaction; guarded by mu. A later sweep of it does not send the denial again (see denyApproval).
	taken map[string]pauseEnd
	// undelivered is the denials that the session took and the agent did not, by interaction, whose tasks are to be cancelled
	// and whose threads told (see cancelUndelivered); guarded by mu.
	undelivered map[string]*undelivered
}

// NewSweeper returns a Sweeper over cfg.
func NewSweeper(cfg SweepConfig) (*Sweeper, error) {
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Lookup == nil || cfg.Session == nil || cfg.Abandoner == nil || cfg.Updaters == nil || cfg.Outbox == nil || cfg.Auditor == nil || cfg.Presses == nil {
		return nil, errors.New("interact: Store, Sessions, Lookup, Session, Abandoner, Updaters, Outbox, Auditor and Presses are required")
	}
	if cfg.OnTimeout == nil {
		return nil, errors.New("interact: OnTimeout is required")
	}
	if cfg.OnApprovalOutcome == nil {
		return nil, errors.New("interact: OnApprovalOutcome is required")
	}
	if cfg.Reopened == nil {
		return nil, errors.New("interact: Reopened is required: without it a denial that is not delivered leaves the task paused")
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
	if cfg.OrphanGrace <= 0 {
		cfg.OrphanGrace = defaultOrphanGrace
	}
	return &Sweeper{cfg: cfg, try: map[string]int{}, downSaid: map[string]bool{}, orphanSince: map[string]time.Time{}, audited: map[string]timeoutAudit{}, taken: map[string]pauseEnd{}, undelivered: map[string]*undelivered{}}, nil
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
// prompt, enqueues the notice, and records the timeout. A step that fails leaves the interaction pending for the next
// sweep. It goes on past one it cannot finish, and returns every such error joined. An interaction whose chat connection is not
// running is left pending, and not counted against Batch (down). An interaction that has no Binding in the config, or whose chat
// connection is not served, cannot be timed out as above, and is ended without the agent once it has been seen so for the OrphanGrace
// (orphaned, endOrphan). For an enabled tool approval, the
// timeout decision is audited before the denial reaches the session; the audit is skipped on retries in this process.
//
// Then it cancels the task of each denial the session took and the agent did not (cancelUndelivered), which is how a timeout
// that denied an approval ends when the denial cannot be delivered.
//
// It lists only pending rows, and an answer is recorded only on a pending row before its deadline (SubmitInteraction), so no
// person's answer can exist for a row when it is listed. One recorded in between wins at the record of the timeout
// (ResolveInteraction answers ErrConflict, and no timeout is counted; an enabled approval may already have its timeout audit)
// by then the pause was ended, the prompt was edited to timed out and the notice enqueued, though the record says the person
// decided. When that person wins after the timeout audit, the sweep appends a superseded audit for its own decision. A press recorded by the deadline
// holds the sweep off (Presses), which is what makes that window as short as the grace.
//
// A question's pause is ended by cancelling its task. A tool approval's (the decoded request's Kind, never the row's: a
// sub-agent's is stored as nested) is ended by denying every tool through the Binding's dialect (spec 8.4 step 6) only on a
// Binding that allows approvals (Binding.ToolApprovals); on any other it is ended as a question's is, with a question's notice, and is
// not counted as an approval's outcome.
func (s *Sweeper) Sweep(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	var errs []error
	due, err := s.cfg.Store.ListDueInteractions(ctx, now.Add(-s.cfg.Grace))
	if err != nil {
		errs = append(errs, fmt.Errorf("interact: list the interactions that are due: %w", err))
	}
	done := 0
	downNow := map[string]bool{} // the connections that do not run and have an interaction that is due, in this sweep
	seen := map[string]bool{}    // the interactions looked at, to drop what is remembered of the others
	looked := err == nil         // every due interaction was looked at (none was, if the listing failed)
	for _, row := range due {
		if done == s.cfg.Batch {
			looked = false
			break
		}
		seen[row.ID] = true
		why, binding := s.orphaned(ctx, row, now)
		if why == notOrphaned {
			delete(s.orphanSince, row.ID) // the Binding is back, or the connection served again: the count starts over if it goes again
		}
		if why != notOrphaned && s.waitsOut(ctx, row, why, now) {
			continue // not counted against the batch: it is not work
		}
		if why == notOrphaned && s.down(ctx, row, downNow) {
			continue // not counted against the batch: it is not work
		}
		switch waiting, err := s.cfg.Presses.PendingPresses(ctx, row.ID, row.Deadline); { // if it cannot be known, wait
		case err != nil:
			errs = append(errs, fmt.Errorf("interact: presses of %s: %w", row.ID, err))
			continue
		case waiting:
			continue // not counted against the batch: it is not work
		}
		done++
		end := func() error { return s.timeOut(ctx, row, now) }
		if why != notOrphaned {
			end = func() error { return s.endOrphan(ctx, row, why, binding, now) }
		}
		if err := end(); err != nil {
			errs = append(errs, err)
		}
	}
	if looked {
		for id := range s.orphanSince {
			if !seen[id] {
				delete(s.orphanSince, id) // settled, or no longer due
			}
		}
		// a connection that nothing due waits on, or that runs, is said again if it stops
		for conn := range s.downSaid {
			if !downNow[conn] {
				delete(s.downSaid, conn)
			}
		}
	}
	// After the timeouts, so that a denial the session took and the agent did not, in this very sweep, is found timed out.
	errs = append(errs, s.cancelUndelivered(ctx)...)
	return errors.Join(errs...)
}

// down says whether the chat connection of the interaction's thread is not running (it has not started, stopped, or was removed), in
// which case the timeout waits for it. Ending the pause resumes the paused turn: the task is cancelled, or the approval's tools are
// denied and the agent goes on, and its output is rendered into the connection's thread, which no platform can reach. The prompt's
// edit and the notice are for that thread too. The interaction stays pending, and is timed out by the first sweep after the
// connection runs (its deadline has passed already); the turn stays paused until then, as a connection's turns are not resumed until
// it runs (session.Manager.Recover). It says so once for each connection, at WARN, and again after a sweep in which no due interaction waited on it (Sweep).
// A platform that cannot be had for another reason is not waited for: the timeout goes on, and its
// edit fails and is tried again as before. A connection that is not served and a Binding that is gone are not "down for now": they are
// told apart first (orphaned), and a connection that is served and does not run keeps its interactions however long that takes.
func (s *Sweeper) down(ctx context.Context, row store.Interaction, downNow map[string]bool) bool {
	if _, err := s.cfg.Updaters.Platform(row.Key.Connection); !errors.Is(err, chat.ErrConnectionDown) {
		return false
	}
	downNow[row.Key.Connection] = true
	if !s.downSaid[row.Key.Connection] {
		s.downSaid[row.Key.Connection] = true
		s.cfg.Logger.WarnContext(ctx, "interact: the chat connection of interactions that are due is not running; they are not timed out until it runs", "connection", row.Key.Connection, "interaction", row.ID)
	}
	return true
}

// orphanReason says why the pause of a due interaction cannot be ended through its Binding and its connection, as a timeout ends it.
type orphanReason int

const (
	// notOrphaned: the Binding is in the config and the connection is served, however it runs now.
	notOrphaned orphanReason = iota
	// bindingGone: the Binding of the interaction's thread is not in the config (it was deleted, or it is not served: its connection was
	// taken away or is not among those this broker serves, or it is rejected with no good generation). Session.Cancel needs its backend and
	// fails, at every sweep, for as long as it is missing.
	bindingGone
	// connectionNotServed: the Binding is in the config, and this broker does not serve the chat connection of the interaction's thread (a second
	// connection of a Slack app another connection uses, or one that is not in the config: the Binding was pointed at another connection and this
	// one deleted, say). Nothing posts for the connection and nothing answers for it, but the agent can be asked.
	connectionNotServed
)

// orphaned says whether the pause of the due interaction row cannot be ended as a timeout ends it (see orphanReason), and the name of the
// Binding of its thread ("" if the thread has no session). An error that does not say the Binding is unknown (the config has not loaded, say) is no
// finding, and nor is a session that cannot be read; the timeout goes on and meets it as before. A denial the session took in an earlier sweep of this
// row (taken) stands, whatever the config says now, and its row is not an orphan's.
func (s *Sweeper) orphaned(ctx context.Context, row store.Interaction, now time.Time) (orphanReason, string) {
	if _, taken := s.taken[row.ID]; taken {
		return notOrphaned, ""
	}
	var binding string
	if sess, err := s.cfg.Sessions.GetSession(ctx, row.Key, store.AnyAge, now); err == nil {
		binding = sess.Binding
		if _, err := s.cfg.Lookup.Binding(binding); errors.Is(err, session.ErrUnknownBinding) {
			return bindingGone, binding
		}
	}
	if s.cfg.Served != nil && !s.cfg.Served(row.Key.Connection) {
		return connectionNotServed, binding
	}
	return notOrphaned, binding
}

// waitsOut says whether the orphan row is still inside its grace: the sweep that first finds it so starts the count (and says so once, at INFO), and it is
// ended by the first sweep, after the OrphanGrace, that finds it so still. A sweep that finds it not an orphan drops the count (Sweep does).
func (s *Sweeper) waitsOut(ctx context.Context, row store.Interaction, why orphanReason, now time.Time) bool {
	since, counting := s.orphanSince[row.ID]
	if !counting {
		s.orphanSince[row.ID] = now
		since = now
		s.cfg.Logger.InfoContext(ctx, "interact: an interaction that is due cannot be ended through its Binding, and is ended without the agent if it stays so",
			"interaction", row.ID, "connection", row.Key.Connection, "why", why.words(), "grace", s.cfg.OrphanGrace)
	}
	return now.Sub(since) < s.cfg.OrphanGrace
}

// words says it for a log line.
func (w orphanReason) words() string {
	if w == connectionNotServed {
		return "the chat connection is not served"
	}
	return "the Binding is not in the config"
}

// endOrphan ends the interaction row, whose Binding is not in the config or whose chat connection is not served, without the agent's task
// being ended through the Binding and with nothing posted by the sweep: the Binding's backend is not there to be called, and no platform
// serves the connection to edit the prompt or post a notice. The pause is released (release), so that the parked runner lets go of its
// task and retention can deal with the session, and the interaction is recorded as timed out: its time ran out with nobody answering. Then it is audited (DecisionOrphaned) and counted
// (OnApprovalOutcome with OutcomeOrphaned, whatever the kind, and neither the approval's timed_out nor OnTimeout), once. The order is the timeout's: a
// crash between the release and the record is finished by the next sweep, whose release finds nothing paused. A prompt that was posted
// is edited to its ending by the repair loop (Replacer) if a platform can be had for it, and left otherwise.
func (s *Sweeper) endOrphan(ctx context.Context, row store.Interaction, why orphanReason, binding string, now time.Time) error {
	cancelled, err := s.release(ctx, row, why)
	if err != nil {
		return fmt.Errorf("interact: end the pause of %s without the agent: %w", row.ID, err)
	}
	switch err = s.cfg.Store.ResolveInteraction(ctx, row.ID, store.InteractionTimedOut, now); {
	case errors.Is(err, store.ErrConflict): // decided in the meantime: the first decision stands
		s.cfg.Logger.InfoContext(ctx, "interact: an interaction was decided while the sweep ended it without the agent; the decision stands", "interaction", row.ID)
		s.forget(row.ID)
		return nil
	case errors.Is(err, store.ErrNotFound):
		s.forget(row.ID)
		return nil
	case err != nil:
		return fmt.Errorf("interact: record the end of %s: %w", row.ID, err)
	}
	s.forget(row.ID)
	d := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionOrphaned, At: now}
	if err := s.cfg.Auditor.Decision(ctx, audit.Actor{Binding: binding}, d); err != nil { // as a timeout's is: it is logged, and never undoes the end
		s.cfg.Logger.ErrorContext(ctx, "interact: an interaction that was ended without the agent was not audited", "interaction", row.ID, "err", err)
	}
	s.cfg.OnApprovalOutcome(binding, OutcomeOrphaned)
	s.cfg.Logger.WarnContext(ctx, "interact: an interaction nobody could time out through its Binding was ended; the sweep posted nothing",
		"interaction", row.ID, "binding", binding, "connection", row.Key.Connection, "task", row.TaskID, "why", why.words(), "task_cancelled_at_agent", cancelled)
	return nil
}

// release ends the pause of row's task. When the Binding is in the config (its connection is not served) the agent is asked to cancel the
// task: one Session.Cancel, which tries a transient failure up to three times (session cancelAttempts). That ends the pause too, and frees the agent of a task that waits for input for ever. A cancel that fails does not hold the end up: the pause is released without the agent. When the Binding is gone there is no backend to call, so the pause is released without the agent (Abandoner) and the task stays at the agent until
// the agent's own expiry; Agent Kourier has nothing more to end it with. The release is never given up on: its failure leaves the interaction
// pending, to be tried again. It says whether the agent cancelled the task.
func (s *Sweeper) release(ctx context.Context, row store.Interaction, why orphanReason) (cancelled bool, err error) {
	if why == connectionNotServed {
		took, err := s.cancelled(ctx, row, "the interaction's chat connection is not served")
		if err == nil { // cancelled, or not paused: either way there is no pause to release
			return took, nil
		}
		s.cfg.Logger.WarnContext(ctx, "interact: the agent could not be asked to cancel the task of an interaction whose chat connection is not served, so the pause is released without it and the task is left at the agent",
			"interaction", row.ID, "task", row.TaskID, "err", err)
	}
	switch err := s.cfg.Abandoner.Abandon(ctx, row.Key, a2a.TaskID(row.TaskID), "the interaction's Binding is not in the config or its chat connection is not served"); {
	case err == nil, errors.Is(err, session.ErrNotPaused): // released, or nothing is paused on the task any more
		return false, nil
	default:
		return false, err
	}
}

// pauseEnd is how the pause of an interaction was ended, which says what its thread is told and whether it is counted as the
// timeout of a tool approval.
type pauseEnd struct {
	notice string
	// binding is the name of the Binding the approval's thread is under, and counted is whether the timeout is counted as an
	// approval's outcome: only for an approval on a Binding that allows them, whose name is known.
	binding string
	counted bool
	// audited says the timeout decision was written before the approval denial (or fallback cancel) was attempted.
	audited bool
}

// timeoutAudit identifies the Binding identity under which a timeout decision was written. A
// reload can replace the Binding while a timeout is being retried, so a changed identity needs a
// fresh audit before the denial is sent.
type timeoutAudit struct {
	binding, identity string
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
	// The order matters: an enabled approval's timeout audit is written before its denial reaches the session; the pause is
	// ended next (the task is cancelled, or an approval's tools are denied), so that nothing is said or edited for a task that
	// is still paused; and the timeout is recorded last, so that a sweep that dies on the way is finished by the next.
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
	case errors.Is(err, store.ErrConflict): // decided in the meantime
		s.supersedeTimeout(ctx, row, now)
		s.forget(row.ID)
		return nil
	case errors.Is(err, store.ErrNotFound):
		s.forget(row.ID)
		return nil
	case err != nil:
		return fmt.Errorf("interact: record the timeout of %s: %w", row.ID, err)
	}
	s.forget(row.ID)
	s.report(ctx, row, end, now)
	return nil
}

// supersedeTimeout compensates the timeout audit when a person's answer wins after the sweep's denial and before its
// ResolveInteraction. The interaction row is the first-complete decision point, while the audit was intentionally written
// before the denial could be sent. The compensation makes the append-only audit log describe that the timeout lost the race;
// failures are logged because the answer already won and retrying the timeout would only risk another denial.
func (s *Sweeper) supersedeTimeout(ctx context.Context, row store.Interaction, now time.Time) {
	marker, ok := s.audited[row.ID]
	if !ok {
		return
	}
	current, err := s.cfg.Store.GetInteraction(ctx, row.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		s.cfg.Logger.ErrorContext(ctx, "interact: could not check the winner after a timeout race", "interaction", row.ID, "err", err)
		return
	case current.State != store.InteractionAnswered:
		return
	}
	decision := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionSuperseded, At: now}
	if err := s.cfg.Auditor.Decision(ctx, audit.Actor{Binding: marker.binding, Identity: marker.identity}, decision); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "interact: a timeout race was not audited as superseded", "interaction", row.ID, "err", err)
	}
}

// forget drops what the sweeper remembers of an interaction whose timeout is recorded or was decided otherwise.
func (s *Sweeper) forget(id string) {
	delete(s.try, id)
	delete(s.orphanSince, id)
	delete(s.audited, id)
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
	_, err := s.cancelled(ctx, row, "the interaction timed out")
	return err
}

// cancelled is cancel, for reason (the session's log), and says whether the session cancelled the task (false: it was not
// paused).
func (s *Sweeper) cancelled(ctx context.Context, row store.Interaction, reason string) (bool, error) {
	err := s.cfg.Session.Cancel(ctx, row.Key, a2a.TaskID(row.TaskID), reason)
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
// A denial the session took in an earlier sweep of this row (taken) stands, whatever the Binding says now: it was sent, so
// the task is not cancelled, nor the thread told that a question was closed, if a later step failed and the Binding is gone or
// its flag is off by the next sweep.
//
// Otherwise only a Binding that allows approvals (Binding.ToolApprovals) has its approval denied. For any other, and for one
// whose Binding is not in the config or whose thread has no session to name it, the task is cancelled as a question's is, with a
// question's notice and no count; that is what the sweep did before approvals could be decided, and it is logged when the
// Binding was expected. A session that cannot be read says nothing of the Binding, and is an error, to be tried again.
func (s *Sweeper) endApproval(ctx context.Context, row store.Interaction, in dialect.Interaction, now time.Time) (pauseEnd, error) {
	if end, ok := s.taken[row.ID]; ok {
		return end, nil
	}
	_, alreadyAudited := s.audited[row.ID]
	asked := pauseEnd{notice: timedOutNotice, audited: alreadyAudited}
	sess, err := s.cfg.Sessions.GetSession(ctx, row.Key, store.AnyAge, now)
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
		// The Binding is missing, for a reason other than the one orphaned finds (the config has not loaded, or its backend cannot be
		// built), or left the config since orphaned looked. The attempt to cancel needs the Binding's backend (session.Manager.Cancel reads it
		// from the same config), so with the real session it fails for as long as that is so; the sweep that finds the Binding
		// unknown for the OrphanGrace ends the row without the agent.
		s.cfg.Logger.WarnContext(ctx, "interact: the Binding of a timed-out approval is not available, so its task is cancelled if it can be", "interaction", row.ID, "binding", sess.Binding, "err", err)
		return asked, s.cancel(ctx, row)
	case !b.ToolApprovals:
		return asked, s.cancel(ctx, row)
	}
	return s.denyApproval(ctx, row, in, b, sess.Binding, now)
}

// denyApproval sends the denial of every tool of the approval in, under the Binding b (named binding), with Agent Kourier
// as the attribution and the message ID the interaction's answer would have (AnswerMessage). It names the approval's task,
// so a newer one the thread has moved on to is not touched. The denial is rebuilt from the stored request, and is not
// stored as the row's answer, which is a person's.
//
// What Session.Answer says is what the sweep learns of the send, and no more: the session sends after it returns. nil: the
// session took the denial. Whether the agent received it is not known here. If it did not, the session reopens the pause and
// tells Reopened, and cancelUndelivered, on a sweep after this row is timed out, cancels the task and tells the thread. A
// denial that is delivered is never followed by a cancel, and the denial is not sent again for it (taken). ErrNotPaused: nothing
// was sent, as the task was not paused on that task, or the session has not parked on it yet; it is not remembered as taken, so
// a later sweep of the same row, if a step fails, asks again. ErrInvalid is final, and a request the dialect will not answer is
// too, and no retry would mend either: the task is cancelled instead, as a question's is. Any other error is returned, and the
// denial is tried again; the essential step is never given up on. No session the broker wires answers any other error today
// (the sweeper has the manager itself, and the manager answers only the two above), so that branch is defensive.
//
// A denial the session took is remembered, and a later sweep of the same row (a later step failed) does not send it again:
// sent again, it could meet a newer pause of the same task, which the session matches by task ID alone, and kagent answers
// tool IDs it does not know by wedging the task. UNCONFIRMED: a restart between the denial and the record, a kagent that
// pauses the same task twice, and what it does with a repeated message ID.
func (s *Sweeper) denyApproval(ctx context.Context, row store.Interaction, in dialect.Interaction, b Binding, binding string, now time.Time) (pauseEnd, error) {
	denied := pauseEnd{notice: timedOutDeniedNotice, binding: binding, counted: true, audited: true}
	cancelled := func() (pauseEnd, error) { // the fallback
		end := pauseEnd{notice: timedOutCancelNotice, binding: binding, counted: true, audited: true}
		took, err := s.cancelled(ctx, row, "the interaction timed out")
		if err == nil && !took {
			end.notice = timedOutNothingNotice
		}
		return end, err
	}
	marker, alreadyAudited := s.audited[row.ID]
	if !alreadyAudited || marker.binding != binding || marker.identity != b.Identity {
		actor := audit.Actor{Binding: binding, Identity: b.Identity}
		decision := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionTimedOut, At: now}
		if err := s.cfg.Auditor.Decision(ctx, actor, decision); err != nil {
			return pauseEnd{}, fmt.Errorf("audit the timeout of %s: %w", row.ID, err)
		}
		s.audited[row.ID] = timeoutAudit{binding: binding, identity: b.Identity}
	}
	msg, err := AnswerMessage(b.Dialect, row.ID, in, denyAll(in, timeoutDenyReason), autoDecider, b.Platform)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "interact: the dialect would not build the denial of a timed-out approval, so its task is cancelled", "interaction", row.ID, "err", err)
		return cancelled()
	}
	switch err := s.cfg.Session.Answer(ctx, row.Key, &msg); {
	case err == nil:
		s.taken[row.ID] = denied
		return denied, nil
	case errors.Is(err, session.ErrNotPaused):
		return pauseEnd{notice: timedOutClosedNotice, binding: binding, counted: true, audited: true}, nil
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
	u, err := s.cfg.Updaters.Platform(row.Key.Connection)
	if err != nil {
		return fmt.Errorf("interact: the platform to edit the prompt of %s: %w", row.ID, err)
	}
	return u.Update(ctx, u.RefAt(row.Key.Channel, row.Key.Thread, row.MessageRef), chat.End(e))
}

// notify enqueues the timeout's notice, text. Its ID comes from the interaction, so the same one is never posted twice.
func (s *Sweeper) notify(ctx context.Context, row store.Interaction, text string) error {
	return s.notifyAs(ctx, row, row.ID, text)
}

// notifyAs is notify with an outbox ID that comes from source, for a notice of the interaction that is not the timeout's.
func (s *Sweeper) notifyAs(ctx context.Context, row store.Interaction, source, text string) error {
	return enqueueNotice(ctx, s.cfg.Outbox, row.Key, source, text)
}

// report audits a timeout this sweep recorded and counts it: OnTimeout for any interaction, and OnApprovalOutcome for the
// tool approval of a Binding that allows them, under the Binding's name that the sweep found in ending the pause (so that a
// failed read of the session now does not leave the count without it). Enabled approvals are audited before their denial;
// other timeout paths are audited after the timeout is recorded, so a failure there is logged and never undoes it (spec 8.6
// step 7). The writer meters the failure itself.
func (s *Sweeper) report(ctx context.Context, row store.Interaction, end pauseEnd, now time.Time) {
	if !end.audited {
		var actor audit.Actor // no person decided it
		if sess, err := s.cfg.Sessions.GetSession(ctx, row.Key, store.AnyAge, now); err == nil {
			actor.Binding = sess.Binding
		} else {
			actor.Binding = end.binding
		}
		d := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionTimedOut, At: now}
		if err := s.cfg.Auditor.Decision(ctx, actor, d); err != nil {
			s.cfg.Logger.ErrorContext(ctx, "interact: a timeout was not audited", "interaction", row.ID, "err", err)
		}
	}
	if end.counted {
		s.cfg.OnApprovalOutcome(end.binding, OutcomeTimedOut)
	}
	s.cfg.OnTimeout(row.ID)
}

// undelivered is a denial that the session took and the agent did not, for which the task is cancelled and the thread told.
type undelivered struct {
	reopened
	cancelled bool // the session cancelled the task; it is not asked again
	told      bool // the notice is enqueued, or given up on
	tries     int  // sweeps that failed to enqueue the notice
	// missingSince is when a cancel first failed because the Binding is not in the config, and every cancel since has; zero if the last did not.
	// It is the orphan's count (SweepConfig.OrphanGrace), kept here and not in orphanSince, which Sweep prunes of the rows that are not due.
	missingSince time.Time
	left         bool // the pause was released without the agent: the task is left with it, and the notice says so
}

// cancelUndelivered cancels the task of each approval whose denial the session took and then failed to deliver (Reopened), and
// tells its thread, once (undeliveredDenialNotice, with an outbox ID of its own: the interaction's, and a suffix). It is the
// bound of a timeout's denial: one attempt, and if the pause is open again, the cancel, which is audited by the session as any
// cancel is (an agent call, naming no person) and is no new decision: the timeout was audited and counted, once, by the sweep
// that recorded it, and is not counted again.
//
// It acts on an approval whose timeout is recorded. One that is still pending waits for the sweep that records it (this runs
// after that), and one that is answered, or is not an approval, was a person's answer or no denial, and is dropped: a person's
// answer that was not delivered is the session's alone. The cancel names the task, so a thread that has moved on to a newer one
// is left alone (ErrNotPaused, and nothing is said). UNCONFIRMED: a task resumed outside Agent Kourier that pauses again
// within one sweep of the failed denial is the same task, and is cancelled.
//
// The cancel is never given up on: it is tried again each sweep, and the notice, which says it is under way, goes out with the
// first try whether or not it works. After a restart nothing is remembered, and the pause that comes again is denied again
// (Intake.Paused), and cancelled here if that fails too: so the denial is tried once more, never in a loop.
func (s *Sweeper) cancelUndelivered(ctx context.Context) []error {
	for id, p := range s.cfg.Reopened.take() {
		if _, ok := s.undelivered[id]; !ok {
			s.undelivered[id] = &undelivered{reopened: p}
		}
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(s.undelivered)) {
		if err := s.cancelOne(ctx, id, s.undelivered[id]); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (s *Sweeper) cancelOne(ctx context.Context, id string, u *undelivered) error {
	row, err := s.cfg.Store.GetInteraction(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		delete(s.undelivered, id)
		return nil
	case err != nil:
		return fmt.Errorf("interact: read %s, whose denial was not delivered: %w", id, err)
	case !denial(row) || row.TaskID != string(u.task):
		delete(s.undelivered, id) // a person's answer, or no denial of this task
		return nil
	case row.State == store.InteractionPending:
		return nil // its timeout is not recorded yet
	}
	var errs []error
	if !u.cancelled {
		took, err := s.cancelled(ctx, row, "the denial of the timed-out approval was not delivered")
		released := false
		if !errors.Is(err, session.ErrUnknownBinding) {
			u.missingSince = time.Time{} // the Binding is there: a cancel that fails now fails for another reason, and is tried again
		} else {
			// The Binding was deleted after the denial was taken: there is no backend to cancel through. A Binding that is back in a moment (a
			// reload that replaces it) lets the cancel through, so for the OrphanGrace the cancel is tried again at each sweep, and nothing is said
			// of it. Past that the pause is released without the agent, whose task is left, as for any pause whose Binding is gone.
			now := s.cfg.Now()
			if u.missingSince.IsZero() {
				u.missingSince = now
				s.cfg.Logger.InfoContext(ctx, "interact: the Binding of a denial that was not delivered is not in the config; the task is cancelled if it is back, and the pause released without the agent if it stays so",
					"interaction", id, "grace", s.cfg.OrphanGrace)
			}
			if now.Sub(u.missingSince) < s.cfg.OrphanGrace {
				return nil
			}
			released = true
			switch err = s.cfg.Abandoner.Abandon(ctx, row.Key, a2a.TaskID(row.TaskID), "the denial of the timed-out approval was not delivered and its Binding is not in the config"); {
			case err == nil:
				took, u.left = true, true
			case errors.Is(err, session.ErrNotPaused):
				took, err = false, nil
			}
		}
		switch {
		case err == nil && !took: // not paused any more: someone else ended it, or it moved on
			s.cfg.Logger.InfoContext(ctx, "interact: the task of a denial that was not delivered is no longer paused, so it was left alone", "interaction", id, "task", row.TaskID)
			delete(s.undelivered, id)
			return nil
		case err != nil:
			errs = append(errs, fmt.Errorf("interact: cancel the task of %s, whose denial was not delivered: %w", id, err))
		default:
			u.cancelled = true
			if released {
				s.cfg.Logger.WarnContext(ctx, "interact: the agent did not take the denial of a timed-out approval and its Binding is gone, so the pause was released and the task is left at the agent", "interaction", id, "task", row.TaskID)
			} else {
				s.cfg.Logger.WarnContext(ctx, "interact: the agent did not take the denial of a timed-out approval, so its task was cancelled", "interaction", id, "task", row.TaskID)
			}
		}
	}
	if !u.told {
		text := undeliveredDenialNotice
		if u.left {
			text = undeliveredLeftNotice
		}
		if err := s.notifyAs(ctx, row, id+"/undelivered", text); err == nil {
			u.told = true
		} else if u.tries++; u.tries >= maxAttempts { // as the edit and the notice of a timeout are
			s.cfg.Logger.ErrorContext(ctx, "interact: giving up on the notice of a denial that was not delivered", "interaction", id, "err", err)
			u.told = true
		} else {
			errs = append(errs, fmt.Errorf("interact: tell the thread that the denial of %s was not delivered: %w", id, err))
		}
	}
	if u.cancelled && u.told {
		delete(s.undelivered, id)
	}
	return errors.Join(errs...)
}
