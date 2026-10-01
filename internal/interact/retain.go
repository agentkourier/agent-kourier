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

// RetentionConfig configures a Retention. Store is required.
type RetentionConfig struct {
	Store PressPurger
	// Interval is how often presses are purged (default 10 minutes).
	Interval time.Duration
	// Grace is how long after its interaction's deadline a press is kept (default 1 hour): the worker's own
	// cap on a press is an hour from its recording, so a press the worker or the timeout sweep still has use
	// for is not purged.
	Grace  time.Duration
	Now    func() time.Time // time.Now if nil
	Logger *slog.Logger     // slog.Default() if nil
}

// Retention is the backstop for the press store: a press is deleted when it is processed, or when the
// worker gives up on it, and this deletes the ones that neither ever happened to (a press recorded for an
// interaction that is gone, a worker that never ran). It is safe to purge a press the worker is on: the
// worker holds the press in memory, and deleting one that is already gone is not an error (DeletePress).
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Retention{cfg}, nil
}

// Run purges every Interval, and not before the first, until ctx ends, and returns its error. A purge that
// fails is logged and tried again at the next tick.
func (r *Retention) Run(ctx context.Context) error {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		n, err := r.cfg.Store.PurgePresses(ctx, r.cfg.Now(), r.cfg.Grace)
		switch {
		case err != nil && ctx.Err() == nil:
			r.cfg.Logger.WarnContext(ctx, "interact: presses could not be purged; it will be tried again", "err", err)
		case n > 0:
			r.cfg.Logger.InfoContext(ctx, "interact: purged presses past their interaction's deadline", "purged", n, "grace", r.cfg.Grace)
		}
	}
}
