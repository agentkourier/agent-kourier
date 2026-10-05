package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
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
)

// MinAuditRetention is the shortest audit retention that is accepted, so that a typo (an hour for a month) does not
// erase the log a day at a time.
const MinAuditRetention = 24 * time.Hour

// What the retention loop counts as removed (courier_retention_removed_total, label what). The interactions the
// interact package clears are counted by its own hook, under RemovedClearedInteractions.
const (
	RemovedStrandedReplies     = "stranded_replies"
	RemovedClearedInteractions = "cleared_interactions"
	RemovedAuditEntries        = "audit_entries"
)

// retentionStore is what the retention loop asks of the store: store.ReplyQueue's backstop and store.AuditRetention.
type retentionStore interface {
	AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (int, error)
	PurgeAudit(ctx context.Context, before time.Time, limit int) (int, error)
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
	// OnRemoved is told what each pass removed (RemovedStrandedReplies, RemovedAuditEntries) and how many, when it
	// removed any, for a metric. Optional.
	OnRemoved func(what string, n int)
	Now       func() time.Time // time.Now if nil
	Logger    *slog.Logger     // slog.Default() if nil
}

// retention is the time backstop of what the store keeps that no other loop owns: the text of replies that were
// never delivered, and, if the operator sets a retention, old audit entries. It writes nothing to the audit log:
// what it removes is counted in the log of the process, and that is all.
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
