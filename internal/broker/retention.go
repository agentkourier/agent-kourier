package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// The retention loop's defaults.
const (
	// defaultStrandedReplyAge is how old an undelivered reply must be before the loop gives it up (see
	// store.ReplyQueue.AbandonStrandedReplies). A turn waits, by design, behind a task that runs or is paused on a
	// question (which the store leaves alone however old), and a busy or not-ready session is waited out without
	// limit; none of those is a matter of weeks. 30 days is long past any of them, and a third of the default
	// threadTTL, after which a thread's session is expired and its queue is never read again.
	defaultStrandedReplyAge = 30 * 24 * time.Hour
	// defaultAuditBatch is how many entries one purge call deletes: small enough that one is a short transaction,
	// under Postgres's statement timeout and SQLite's one writer.
	defaultAuditBatch = 1000
	// defaultMaxAuditBatches bounds the calls of one tick, so that a log far past its retention is worked down over
	// several ticks and the loop stays stoppable: a million entries.
	defaultMaxAuditBatches = 1000
	// defaultSessionGrace is how long after its threadTTL has run out a session is kept before it is deleted with
	// everything that belongs to it. An expired session is kept so that a reply in its thread gets the expiry notice
	// rather than silence (spec 8.2); that is a courtesy to a person who comes back, and 90 days is long past the
	// weeks in which anyone does, while a thread that is never answered again costs its rows for ever.
	defaultSessionGrace = 90 * 24 * time.Hour
	// defaultSessionBatch is how many sessions one purge call deletes, each with the rows that belong to it (replies,
	// interactions, outbox rows): one transaction of a few thousand rows at most, under Postgres's statement timeout and
	// SQLite's one writer.
	defaultSessionBatch = 100
	// defaultMaxSessionBatches bounds the calls of one tick, so that a backlog (the first run after an upgrade finds every
	// session ever made) is worked down over several ticks: ten thousand sessions.
	defaultMaxSessionBatches = 100
	// defaultCancelTimeout bounds the cancel of one stale task at its agent. It is a courtesy to the agent: the task is
	// cleared whether or not it comes back, and the sweep must not wait on an agent that does not answer.
	defaultCancelTimeout = 10 * time.Second
	// defaultStaleBatch is how many stale tasks one listing reads, and defaultMaxStaleBatches how many listings one tick makes:
	// five hundred tasks, each a call at an agent that may be slow, which is far more than a crash leaves.
	defaultStaleBatch      = 50
	defaultMaxStaleBatches = 10
)

// MinAuditRetention is the shortest audit retention that is accepted, so that a typo (an hour for a month) does not
// erase the log a day at a time.
const MinAuditRetention = 24 * time.Hour

// What the retention loop counts as removed (agentkourier_retention_removed_total, label what). The interactions the
// interact package clears are counted by its own hook, under RemovedClearedInteractions. A deleted session is counted
// under RemovedExpiredSessions, and each kind of row that went with it under its own. A task that a crash left on a
// session that was past its horizon, which the loop ended so that the session could be deleted, is counted under
// RemovedStaleTasks, and the pending interactions that were cancelled with it under RemovedStaleInteractions.
const (
	RemovedStrandedReplies     = "stranded_replies"
	RemovedClearedInteractions = "cleared_interactions"
	RemovedAuditEntries        = "audit_entries"
	RemovedExpiredSessions     = "expired_sessions"
	RemovedSessionReplies      = "session_replies"
	RemovedSessionInteractions = "session_interactions"
	RemovedSessionPresses      = "session_presses"
	RemovedSessionOutbox       = "session_outbox"
	RemovedStaleTasks          = "stale_tasks"
	RemovedStaleInteractions   = "stale_task_interactions"
)

// retentionStore is what the retention loop asks of the store: store.ReplyQueue's backstop, the acknowledgments it
// retries (store.AcknowledgmentLister, store.PendingAcknowledgmentLister), store.AuditRetention, store.SessionRetention
// and store.StaleTasks.
type retentionStore interface {
	AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (int, error)
	store.AcknowledgmentLister
	store.PendingAcknowledgmentLister
	store.AuditRetention
	store.SessionRetention
	store.StaleTasks
}

// taskCanceller cancels a task at the agent of a Binding: Bindings. It returns the service identity the call was made
// under (empty when none was made), and its error wraps session.ErrUnknownBinding when the config has no such Binding.
type taskCanceller interface {
	CancelTask(ctx context.Context, binding, taskID string) (identity string, err error)
}

// staleAuditor records a stale task that was ended: audit.Writer.
type staleAuditor interface {
	StaleTaskEnded(ctx context.Context, a audit.Actor, t audit.StaleTask) error
}

// retentionConfig configures a retention. Store, ThreadTTLs, Canceller, Audit, Holds and OnRemoved are required:
// newRetention refuses a config without them and names the field, so that a wiring that leaves one out fails at start-up
// and the loop does not run with a pass that does nothing. The rest are tunables with defaults.
type retentionConfig struct {
	Store retentionStore
	// Interval is how often the loop runs (default 1 hour).
	Interval time.Duration
	// ReplyAge is how old an undelivered reply is before it is given up (default defaultStrandedReplyAge).
	ReplyAge time.Duration
	// AuditRetention is how long audit entries are kept; zero keeps them for ever (the setting "keep"), and a
	// shorter one than MinAuditRetention is refused.
	AuditRetention time.Duration
	// AuditBatch is how many entries each purge call deletes (default 1000), and MaxAuditBatches how many calls one
	// tick makes at most (default 1000).
	AuditBatch, MaxAuditBatches int
	// ThreadTTLs returns the threadTTL of each Binding of the config by "namespace/name", and false while no config
	// is loaded: Bindings.ThreadTTLs. A session is deleted SessionGrace after its Binding's threadTTL.
	ThreadTTLs func() (map[string]time.Duration, bool)
	// SessionGrace is how long past its threadTTL a session is kept (default defaultSessionGrace).
	SessionGrace time.Duration
	// SessionBatch is how many sessions each purge call deletes (default 100), and MaxSessionBatches how many calls one
	// tick makes at most (default 100).
	SessionBatch, MaxSessionBatches int
	// Canceller cancels a stale task at its agent, and Audit records that the task was ended.
	Canceller taskCanceller
	Audit     staleAuditor
	// Holds reports whether this process's session manager still runs the task on the session: parked on a question it
	// has not given up on, or streaming it. A task it holds is left alone, for its own runner and the interaction timeout
	// to end: clearing it from under the runner would leave the runner parked on it after the session is deleted, and a
	// new session under the same key would wait behind it until a restart.
	Holds func(key store.ConversationKey, taskID string) bool
	// CancelTimeout bounds each cancel at an agent (default defaultCancelTimeout).
	CancelTimeout time.Duration
	// StaleBatch is how many stale tasks each listing reads (default 50), and MaxStaleBatches how many listings one tick
	// makes at most (default 10).
	StaleBatch, MaxStaleBatches int
	// OnRemoved is told what each pass removed (the Removed constants) and how many, when it removed any, for a metric.
	OnRemoved func(what string, n int)
	// EnsureAcknowledgment queues a durable add for a queued reply. It is optional for tests and wirings that do not
	// run a chat sink; the leader wires it to the renderer's outbox path so a transient intake failure is retried.
	EnsureAcknowledgment func(context.Context, store.Reply) error
	// ClearAcknowledgment queues a durable removal for a delivered reply. It is optional for tests and wirings that do
	// not run a chat sink; the leader wires it to the renderer's outbox path.
	ClearAcknowledgment func(context.Context, store.Reply) error
	Now                 func() time.Time // time.Now if nil
	Logger              *slog.Logger     // slog.Default() if nil
}

// retention is the time backstop of what the store keeps that no other loop owns: the text of replies that were
// never delivered, tasks that a crash left on sessions that have been expired for good, those sessions with everything
// that belongs to them, and, if the operator sets a retention, old audit entries. What it removes is counted and logged
// by count; only the ending of a stale task is written to the audit log, by identifiers.
type retention struct{ cfg retentionConfig }

// newRetention returns a retention over cfg.
func newRetention(cfg retentionConfig) (*retention, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("broker: a Store is required for retention")
	case cfg.Holds == nil: // a stale task is cancelled and cleared only where the runners that may hold it are asked
		return nil, errors.New("broker: Holds is required for retention: a task a runner holds must not be cleared from under it")
	case cfg.ThreadTTLs == nil:
		return nil, errors.New("broker: ThreadTTLs is required for retention: without it no session is deleted and no stale task is ended")
	case cfg.Canceller == nil:
		return nil, errors.New("broker: Canceller is required for retention: a stale task is cancelled at its agent")
	case cfg.Audit == nil:
		return nil, errors.New("broker: Audit is required for retention: an ended stale task is recorded")
	case cfg.OnRemoved == nil:
		return nil, errors.New("broker: OnRemoved is required for retention: what it removes is counted")
	}
	if cfg.AuditRetention < 0 || (cfg.AuditRetention > 0 && cfg.AuditRetention < MinAuditRetention) {
		return nil, fmt.Errorf("broker: the audit retention is %v; want 0 (keep) or at least %v", cfg.AuditRetention, MinAuditRetention)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.ReplyAge <= 0 {
		cfg.ReplyAge = defaultStrandedReplyAge
	}
	if cfg.AuditBatch <= 0 {
		cfg.AuditBatch = defaultAuditBatch
	}
	if cfg.MaxAuditBatches <= 0 {
		cfg.MaxAuditBatches = defaultMaxAuditBatches
	}
	if cfg.SessionGrace <= 0 {
		cfg.SessionGrace = defaultSessionGrace
	}
	if cfg.SessionBatch <= 0 {
		cfg.SessionBatch = defaultSessionBatch
	}
	if cfg.MaxSessionBatches <= 0 {
		cfg.MaxSessionBatches = defaultMaxSessionBatches
	}
	if cfg.CancelTimeout <= 0 {
		cfg.CancelTimeout = defaultCancelTimeout
	}
	if cfg.StaleBatch <= 0 {
		cfg.StaleBatch = defaultStaleBatch
	}
	if cfg.MaxStaleBatches <= 0 {
		cfg.MaxStaleBatches = defaultMaxStaleBatches
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &retention{cfg}, nil
}

// Run does its work every Interval, and not before the first, until ctx ends, and returns its error. A failure is
// logged and tried again at the next tick, and does not stop the rest.
func (r *retention) Run(ctx context.Context) error {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		r.endStaleTasks(ctx) // first, so that what it frees is dropped and deleted by the passes below, in the same tick
		r.abandonReplies(ctx)
		r.purgeSessions(ctx)
		r.purgeAudit(ctx)
	}
}

func (r *retention) abandonReplies(ctx context.Context) {
	now := r.cfg.Now()
	var n int
	var err error
	if r.cfg.EnsureAcknowledgment != nil {
		err = eachReply(ctx, r.cfg.Store.ListPendingAcknowledgmentReplies, r.cfg.EnsureAcknowledgment)
	}
	if err == nil && r.cfg.ClearAcknowledgment != nil {
		err = eachReply(ctx, r.cfg.Store.ListAcknowledgmentReplies, r.cfg.ClearAcknowledgment)
	}
	if err == nil {
		n, err = r.cfg.Store.AbandonStrandedReplies(ctx, now, r.cfg.ReplyAge)
	}
	switch {
	case err != nil && ctx.Err() == nil:
		r.cfg.Logger.WarnContext(ctx, "retention: replies that were never delivered could not be dropped; it will be tried again", "err", err)
	case n > 0: // a turn that was never sent for this long is a fault worth a look, hence a warning
		r.removed(RemovedStrandedReplies, n)
		r.cfg.Logger.WarnContext(ctx, "retention: dropped replies that were never delivered, and their text", "dropped", n, "age", r.cfg.ReplyAge)
	}
}

// eachReply hands each reply that list returns to queue, and stops at the first error.
func eachReply(ctx context.Context, list func(context.Context) ([]store.Reply, error), queue func(context.Context, store.Reply) error) error {
	rows, err := list(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := queue(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

// sessionHorizons is when each Binding's sessions are past deleting: its threadTTL and the grace after now. A session
// whose Binding is not in the config has no threadTTL to go by, and is held to the longest that any Binding has, and to no
// less than the default: a Binding that is missing for a while (a config that lost it, and is mended) must not have its
// sessions deleted on a shorter reckoning than it would have kept them by.
func (r *retention) sessionHorizons(ttls map[string]time.Duration, now time.Time) store.SessionHorizons {
	h := store.SessionHorizons{ByBinding: make(map[string]time.Time, len(ttls))}
	longest := v1alpha1.DefaultThreadTTL
	for name, ttl := range ttls {
		h.ByBinding[name] = now.Add(-ttl - r.cfg.SessionGrace)
		longest = max(longest, ttl)
	}
	h.Otherwise = now.Add(-longest - r.cfg.SessionGrace)
	return h
}

// endStaleTasks ends the tasks that sessions past their horizon still record: only a crash after a session expired leaves
// one (recovery skips an expired session, and a task that ends clears itself), and the purge never deletes a session with a
// task, so without this such a session would be kept for ever. For each, oldest first, it asks the agent to cancel the task,
// bounded and best effort (an unreachable agent, an error or a Binding that has left the config does not stop it), and then
// clears the task, cancels its pending interactions and audits it, which the store does only while the session is still past
// its horizon and still records that task. A session inside its horizon is never listed. It does nothing before a config is
// loaded.
func (r *retention) endStaleTasks(ctx context.Context) {
	ttls, ok := r.cfg.ThreadTTLs()
	if !ok {
		return
	}
	now := r.cfg.Now()
	h := r.sessionHorizons(ttls, now)
	var tally staleTally
	tried := map[store.ConversationKey]bool{} // a session whose ending failed is not tried again in this tick
	down := map[string]bool{}                 // Bindings whose agent did not answer in this tick: not asked again
list:
	for range r.cfg.MaxStaleBatches {
		stale, err := r.cfg.Store.ListStaleTasks(ctx, h, r.cfg.StaleBatch)
		if err != nil {
			if ctx.Err() == nil {
				r.cfg.Logger.WarnContext(ctx, "retention: stale tasks could not be listed; it will be tried again", "err", err)
			}
			break
		}
		fresh := 0
		for _, t := range stale {
			if tried[t.Key] {
				continue
			}
			tried[t.Key] = true
			fresh++
			if r.cfg.Holds(t.Key, t.TaskID) {
				tally.held++
				continue
			}
			cancel := r.cancelStale(ctx, t, down)
			if ctx.Err() != nil { // shutting down: the cancel may not have been made, and the next run makes it
				break list
			}
			ack, err := r.staleAcknowledgment(ctx, t)
			if err != nil {
				if ctx.Err() == nil {
					r.cfg.Logger.WarnContext(ctx, "retention: acknowledgment removal for a stale task could not be queued", "err", err)
				}
				continue
			}
			ended, err := r.cfg.Store.EndStaleTask(ctx, h, t, now)
			switch {
			case err != nil:
				if ctx.Err() == nil {
					r.cfg.Logger.WarnContext(ctx, "retention: a stale task could not be ended; it will be tried again", "err", err)
				}
			case ended.Ended:
				if ack != nil && r.cfg.ClearAcknowledgment != nil {
					if err := r.cfg.ClearAcknowledgment(ctx, *ack); err != nil && ctx.Err() == nil {
						r.cfg.Logger.WarnContext(ctx, "retention: acknowledgment removal for an ended stale task could not be queued", "err", err)
					}
				}
				tally.add(cancel.how, ended.Interactions)
				r.auditStale(ctx, t, cancel, ended.Interactions)
			}
		}
		if len(stale) < r.cfg.StaleBatch || fresh == 0 {
			break
		}
	}
	if tally.held > 0 {
		r.cfg.Logger.WarnContext(ctx, "retention: left stale tasks that this process still runs; their runners, the interaction timeout or a restart end them", "tasks", tally.held)
	}
	if tally.tasks > 0 {
		r.removed(RemovedStaleTasks, tally.tasks)
		if tally.interactions > 0 {
			r.removed(RemovedStaleInteractions, tally.interactions)
		}
		r.cfg.Logger.InfoContext(ctx, "retention: ended tasks that a crash left on sessions that have been expired for good",
			"tasks", tally.tasks, "interactions", tally.interactions, "cancelled", tally.cancelled, "cancel_failed", tally.failed, "cancel_skipped", tally.skipped)
	}
}

func (r *retention) staleAcknowledgment(ctx context.Context, t store.StaleTask) (*store.Reply, error) {
	if r.cfg.ClearAcknowledgment == nil {
		return nil, nil
	}
	turn, err := r.cfg.Store.StaleTaskTurn(ctx, t.Key, t.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if turn.ID == "" || turn.Acknowledgment == "" || turn.MessageRef == "" {
		return nil, nil
	}
	return &store.Reply{ID: cmp.Or(turn.AcknowledgmentID, turn.ID), Key: t.Key, MessageRef: turn.MessageRef, Acknowledgment: turn.Acknowledgment}, nil
}

// staleTally counts what one tick of endStaleTasks did.
type staleTally struct {
	tasks, interactions        int
	held                       int // left to this process's runner (Holds)
	cancelled, failed, skipped int // how the cancels at the agents went: skipped is no Binding, or an agent that did not answer
}

func (s *staleTally) add(cancel audit.StaleCancel, interactions int) {
	s.tasks++
	s.interactions += interactions
	switch cancel {
	case audit.StaleCancelOK:
		s.cancelled++
	case audit.StaleCancelFailed:
		s.failed++
	default:
		s.skipped++
	}
}

// staleCancel is how the cancel of a stale task went: what happened, the agent's error of a failed one, and the identity the
// call was made under, which is empty when no call was made.
type staleCancel struct {
	how      audit.StaleCancel
	err      error
	identity string
}

// cancelStale asks the agent of t's Binding to cancel the task, within CancelTimeout, and says how it went. It never fails
// the sweep. An agent that does not answer once is not asked again in the tick (down): a task of a Binding whose agent is
// down would otherwise cost a timeout each. An agent that answers that the task is unknown or cannot be cancelled is up.
func (r *retention) cancelStale(ctx context.Context, t store.StaleTask, down map[string]bool) staleCancel {
	if down[t.Binding] {
		return staleCancel{how: audit.StaleCancelSkipped}
	}
	cctx, stop := context.WithTimeout(ctx, r.cfg.CancelTimeout)
	defer stop()
	identity, err := r.cfg.Canceller.CancelTask(cctx, t.Binding, t.TaskID)
	switch {
	case err == nil:
		return staleCancel{how: audit.StaleCancelOK, identity: identity}
	case errors.Is(err, session.ErrUnknownBinding):
		return staleCancel{how: audit.StaleCancelNoBinding}
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) && !errors.Is(err, a2a.ErrTaskNotCancelable) {
		down[t.Binding] = true
	}
	return staleCancel{how: audit.StaleCancelFailed, err: err, identity: identity}
}

// auditStale records that t was ended, as the identity the cancel was made under when one was. The task is ended, so a
// failure to audit is never undone by it: the writer logs and meters it, and the sweep goes on.
func (r *retention) auditStale(ctx context.Context, t store.StaleTask, c staleCancel, interactions int) {
	_ = r.cfg.Audit.StaleTaskEnded(ctx, audit.Actor{Binding: t.Binding, Identity: c.identity}, audit.StaleTask{
		TaskID: a2a.TaskID(t.TaskID), ContextID: t.ContextID, Cancel: c.how, CancelErr: c.err, Interactions: interactions})
}

// purgeSessions deletes the sessions past their horizon, a batch at a time until one is short or the tick's share is spent
// (a session with a task, a pending interaction, or anything undelivered is never due, however old; the store decides).
// It does nothing before a config is loaded.
func (r *retention) purgeSessions(ctx context.Context) {
	ttls, ok := r.cfg.ThreadTTLs()
	if !ok {
		return
	}
	h := r.sessionHorizons(ttls, r.cfg.Now())
	var total store.PurgedSessions
	for range r.cfg.MaxSessionBatches {
		got, err := r.cfg.Store.PurgeExpiredSessions(ctx, h, r.cfg.SessionBatch)
		total = total.Plus(got)
		if err != nil {
			r.removedSessions(total) // the batches before it were done
			if ctx.Err() == nil {
				r.cfg.Logger.WarnContext(ctx, "retention: expired sessions could not be deleted; it will be tried again", "err", err, "deleted", total.Sessions)
			}
			return
		}
		if got.Sessions < r.cfg.SessionBatch {
			break
		}
	}
	if total.Sessions > 0 {
		r.removedSessions(total)
		r.cfg.Logger.InfoContext(ctx, "retention: deleted sessions that have been expired for good, with the rows that belonged to them",
			"sessions", total.Sessions, "replies", total.Replies, "interactions", total.Interactions, "presses", total.Presses, "outbox", total.Outbox, "grace", r.cfg.SessionGrace)
	}
}

// removedSessions counts what a purge deleted, a kind of row at a time, and nothing for a kind it found none of.
func (r *retention) removedSessions(p store.PurgedSessions) {
	for _, c := range []struct {
		what string
		n    int
	}{
		{RemovedExpiredSessions, p.Sessions}, {RemovedSessionReplies, p.Replies}, {RemovedSessionInteractions, p.Interactions},
		{RemovedSessionPresses, p.Presses}, {RemovedSessionOutbox, p.Outbox},
	} {
		if c.n > 0 {
			r.removed(c.what, c.n)
		}
	}
}

// purgeAudit deletes the entries before the retention, a batch at a time until one is short or the tick's share is
// spent. It does nothing when the retention is unset.
func (r *retention) purgeAudit(ctx context.Context) {
	if r.cfg.AuditRetention == 0 {
		return
	}
	before := r.cfg.Now().Add(-r.cfg.AuditRetention)
	total := 0
	for range r.cfg.MaxAuditBatches {
		n, err := r.cfg.Store.PurgeAudit(ctx, before, r.cfg.AuditBatch)
		total += n
		if err != nil {
			if total > 0 { // the batches before it were done
				r.removed(RemovedAuditEntries, total)
			}
			if ctx.Err() == nil {
				r.cfg.Logger.WarnContext(ctx, "retention: audit entries past the retention could not be purged; it will be tried again", "err", err, "purged", total)
			}
			return
		}
		if n < r.cfg.AuditBatch {
			break
		}
	}
	if total > 0 {
		r.removed(RemovedAuditEntries, total)
		r.cfg.Logger.InfoContext(ctx, "retention: purged audit entries past the retention", "purged", total, "retention", r.cfg.AuditRetention)
	}
}

func (r *retention) removed(what string, n int) {
	r.cfg.OnRemoved(what, n)
}
