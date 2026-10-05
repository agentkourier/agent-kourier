package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
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
)

// MinAuditRetention is the shortest audit retention that is accepted, so that a typo (an hour for a month) does not
// erase the log a day at a time.
const MinAuditRetention = 24 * time.Hour

// What the retention loop counts as removed (courier_retention_removed_total, label what). The interactions the
// interact package clears are counted by its own hook, under RemovedClearedInteractions. A deleted session is counted
// under RemovedExpiredSessions, and each kind of row that went with it under its own.
const (
	RemovedStrandedReplies     = "stranded_replies"
	RemovedClearedInteractions = "cleared_interactions"
	RemovedAuditEntries        = "audit_entries"
	RemovedExpiredSessions     = "expired_sessions"
	RemovedSessionReplies      = "session_replies"
	RemovedSessionInteractions = "session_interactions"
	RemovedSessionPresses      = "session_presses"
	RemovedSessionOutbox       = "session_outbox"
)

// retentionStore is what the retention loop asks of the store: store.ReplyQueue's backstop, store.AuditRetention and
// store.SessionRetention.
type retentionStore interface {
	AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (int, error)
	PurgeAudit(ctx context.Context, before time.Time, limit int) (int, error)
	PurgeExpiredSessions(ctx context.Context, h store.SessionHorizons, limit int) (store.PurgedSessions, error)
}

// retentionConfig configures a retention. Store is required.
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
	// is loaded: Bindings.ThreadTTLs. A session is deleted SessionGrace after its Binding's threadTTL, so without it
	// the loop does not delete sessions; broker wiring always sets it.
	ThreadTTLs func() (map[string]time.Duration, bool)
	// SessionGrace is how long past its threadTTL a session is kept (default defaultSessionGrace).
	SessionGrace time.Duration
	// SessionBatch is how many sessions each purge call deletes (default 100), and MaxSessionBatches how many calls one
	// tick makes at most (default 100).
	SessionBatch, MaxSessionBatches int
	// OnRemoved is told what each pass removed (the Removed constants) and how many, when it removed any, for a metric.
	// Optional.
	OnRemoved func(what string, n int)
	Now       func() time.Time // time.Now if nil
	Logger    *slog.Logger     // slog.Default() if nil
}

// retention is the time backstop of what the store keeps that no other loop owns: the text of replies that were
// never delivered, sessions that have been expired for good with everything that belongs to them, and, if the
// operator sets a retention, old audit entries. It writes nothing to the audit log: what it removes is counted in the
// log of the process, and that is all.
type retention struct{ cfg retentionConfig }

// newRetention returns a retention over cfg.
func newRetention(cfg retentionConfig) (*retention, error) {
	if cfg.Store == nil {
		return nil, errors.New("broker: a Store is required for retention")
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
		r.abandonReplies(ctx)
		r.purgeSessions(ctx)
		r.purgeAudit(ctx)
	}
}

func (r *retention) abandonReplies(ctx context.Context) {
	n, err := r.cfg.Store.AbandonStrandedReplies(ctx, r.cfg.Now(), r.cfg.ReplyAge)
	switch {
	case err != nil && ctx.Err() == nil:
		r.cfg.Logger.WarnContext(ctx, "retention: replies that were never delivered could not be dropped; it will be tried again", "err", err)
	case n > 0: // a turn that was never sent for this long is a fault worth a look, hence a warning
		r.removed(RemovedStrandedReplies, n)
		r.cfg.Logger.WarnContext(ctx, "retention: dropped replies that were never delivered, and their text", "dropped", n, "age", r.cfg.ReplyAge)
	}
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

// purgeSessions deletes the sessions past their horizon, a batch at a time until one is short or the tick's share is spent
// (a session with a task, a pending interaction, or anything undelivered is never due, however old; the store decides).
// It does nothing before a config is loaded, and with no source of threadTTLs.
func (r *retention) purgeSessions(ctx context.Context) {
	if r.cfg.ThreadTTLs == nil {
		return
	}
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
	if r.cfg.OnRemoved != nil {
		r.cfg.OnRemoved(what, n)
	}
}
