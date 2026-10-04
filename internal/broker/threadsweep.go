package broker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// threadPurger deletes the rows of trigger threads that do nothing: store.TriggerThreads.
type threadPurger interface {
	PurgeTriggerThreads(ctx context.Context, now, idleBefore time.Time) (int, error)
}

// threadSweepConfig configures a threadSweep. Store is required.
type threadSweepConfig struct {
	Store threadPurger
	// Interval is how often rows are purged (default 1 hour).
	Interval time.Duration
	Now      func() time.Time // time.Now if nil
	Logger   *slog.Logger     // slog.Default() if nil
}

// threadSweep is the retention of the trigger_threads table (spec 8.2): a row is kept after its thread closes so that
// the cooldown outlives the close, and an open thread's row stays until a fire takes it over, so without a sweep a key
// that never fires again keeps its row for good. It deletes the closed rows past their cooldown, and the open ones with
// no message for longer than any trigger's maxIdle can be (v1alpha1.MaxThreadMaxIdle): a shorter cutoff would delete a
// thread that a trigger with a long maxIdle still counts as live, and that key's next fire would start a second
// investigation. A key whose row is gone starts afresh, as one with no row does.
type threadSweep struct{ cfg threadSweepConfig }

// newThreadSweep returns a threadSweep over cfg.
func newThreadSweep(cfg threadSweepConfig) (*threadSweep, error) {
	if cfg.Store == nil {
		return nil, errors.New("broker: a Store is required to sweep trigger threads")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &threadSweep{cfg}, nil
}

// Run purges every Interval, and not before the first, until ctx ends, and returns its error. A purge that fails is
// logged and tried again at the next tick.
func (s *threadSweep) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		now := s.cfg.Now()
		n, err := s.cfg.Store.PurgeTriggerThreads(ctx, now, now.Add(-v1alpha1.MaxThreadMaxIdle))
		switch {
		case err != nil && ctx.Err() == nil:
			s.cfg.Logger.WarnContext(ctx, "chat: trigger threads could not be purged; it will be tried again", "err", err)
		case n > 0:
			s.cfg.Logger.InfoContext(ctx, "chat: purged rows of trigger threads that no longer do anything", "purged", n)
		}
	}
}
