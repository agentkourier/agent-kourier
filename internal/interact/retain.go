package interact

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PressPurger deletes presses past their interaction's deadline plus a grace: store.Presses.
type PressPurger interface {
	PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (int, error)
}

// InteractionClearer empties the request and answer of settled interactions: store.Interactions.
type InteractionClearer interface {
	ClearSettledInteractions(ctx context.Context, now time.Time, grace, undeliveredGrace time.Duration) (int, error)
}

// RetentionStore is what Retention keeps tidy: the presses and the settled interactions.
type RetentionStore interface {
	PressPurger
	InteractionClearer
}

// Defaults of the grace periods, each chosen from what still reads the thing it keeps.
const (
	// defaultClearGrace is how long a settled interaction keeps its request and answer: the repair of ended prompts
	// (Replacer) builds its edit from them for RepairHorizon after the decision, and a press that was recorded just
	// before that is processed for up to the worker's cap (defaultGiveUpAfter) after it, so nothing that still
	// needs them runs later than this.
	defaultClearGrace = RepairHorizon + defaultGiveUpAfter
	// defaultUndeliveredGrace is how long an answer that was never marked delivered is kept: it is the only copy of
	// what the session may still take, as it will after a restart that finds the task paused again. A week is longer
	// than a pause waits for an answer, a restart takes, or the backstop (which tells the thread after minutes) needs.
	defaultUndeliveredGrace = 7 * 24 * time.Hour
)

// RetentionConfig configures a Retention. Store is required.
type RetentionConfig struct {
	Store RetentionStore
	// Interval is how often presses are purged and interactions cleared (default 10 minutes).
	Interval time.Duration
	// Grace is how long after its interaction's deadline a press is kept (default 1 hour): the worker's own
	// cap on a press is an hour from its recording, so a press the worker or the timeout sweep still has use
	// for is not purged.
	Grace time.Duration
	// ClearGrace is how long after it was decided a settled interaction keeps its request and answer (default
	// RepairHorizon plus an hour, see defaultClearGrace); UndeliveredGrace is the same for an answered one whose
	// answer was never marked delivered (default 7 days). The rest of the row is kept.
	ClearGrace, UndeliveredGrace time.Duration
	// OnCleared is told how many interactions each sweep cleared, when it cleared any, for a metric. Optional.
	OnCleared func(n int)
	Now       func() time.Time // time.Now if nil
	Logger    *slog.Logger     // slog.Default() if nil
}

// Retention is the backstop for what the interactions leave in the store. A press is deleted when it is
// processed, or when the worker gives up on it, and this deletes the ones that neither ever happened to (a
// press recorded for an interaction that is gone, a worker that never ran). It is safe to purge a press the
// worker is on: the worker holds the press in memory, and deleting one that is already gone is not an error
// (DeletePress). An interaction keeps what the agent wrote and what a person typed only while something reads
// it: once it is settled and its grace has passed, the request and answer are cleared and the row stays
// (store.Interactions.ClearSettledInteractions).
type Retention struct{ cfg RetentionConfig }

// NewRetention returns a Retention over cfg.
func NewRetention(cfg RetentionConfig) (*Retention, error) {
	if cfg.Store == nil {
		return nil, errors.New("interact: a Store is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	if cfg.Grace <= 0 {
		cfg.Grace = time.Hour
	}
	if cfg.ClearGrace <= 0 {
		cfg.ClearGrace = defaultClearGrace
	}
	if cfg.UndeliveredGrace <= 0 {
		cfg.UndeliveredGrace = defaultUndeliveredGrace
	}
	if cfg.UndeliveredGrace < cfg.ClearGrace {
		return nil, errors.New("interact: an undelivered answer cannot be cleared sooner than a delivered one")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Retention{cfg}, nil
}

// Run purges every Interval, and not before the first, until ctx ends, and returns its error. A purge that
// fails is logged and tried again at the next tick, and does not stop the other.
func (r *Retention) Run(ctx context.Context) error {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		now := r.cfg.Now()
		n, err := r.cfg.Store.PurgePresses(ctx, now, r.cfg.Grace)
		switch {
		case err != nil && ctx.Err() == nil:
			r.cfg.Logger.WarnContext(ctx, "interact: presses could not be purged; it will be tried again", "err", err)
		case n > 0:
			r.cfg.Logger.InfoContext(ctx, "interact: purged presses past their interaction's deadline", "purged", n, "grace", r.cfg.Grace)
		}
		n, err = r.cfg.Store.ClearSettledInteractions(ctx, now, r.cfg.ClearGrace, r.cfg.UndeliveredGrace)
		switch {
		case err != nil && ctx.Err() == nil:
			r.cfg.Logger.WarnContext(ctx, "interact: settled interactions could not be cleared; it will be tried again", "err", err)
		case n > 0:
			if r.cfg.OnCleared != nil {
				r.cfg.OnCleared(n)
			}
			r.cfg.Logger.InfoContext(ctx, "interact: cleared the request and answer of settled interactions", "cleared", n, "grace", r.cfg.ClearGrace)
		}
	}
}
