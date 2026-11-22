package agent

import (
	"context"
	"log/slog"
	"time"
)

// UpkeepInterval is how often Upkeep.Run sweeps. A context can outlive SessionIdleTTL, and a task TaskRetention, by
// up to this much.
const UpkeepInterval = time.Minute

// UpkeepStore is what Upkeep deletes from. store.Store implements it.
type UpkeepStore interface {
	// DeleteSettledBefore deletes the settled tasks last updated before cutoff, with their message IDs.
	DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// DeleteIdleContexts deletes each context last active before cutoff, with no turn running: its ADK session, its
	// tasks and their message IDs.
	DeleteIdleContexts(ctx context.Context, cutoff time.Time) (int, error)
}

// Upkeep keeps the store from growing without bound.
type Upkeep struct {
	// Store is what it deletes from.
	Store UpkeepStore
	// TaskRetention is how long a settled task and its message IDs are kept; zero keeps them.
	TaskRetention time.Duration
	// SessionIdleTTL is how long a context may go without a task update before its ADK session and its tasks are
	// deleted; zero keeps them.
	SessionIdleTTL time.Duration
	// Log receives what each sweep deleted, and why one failed.
	Log *slog.Logger
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
}

// Run sweeps every UpkeepInterval until ctx ends. It returns at once when both TaskRetention and SessionIdleTTL are
// zero.
func (u *Upkeep) Run(ctx context.Context) {
	if u.TaskRetention <= 0 && u.SessionIdleTTL <= 0 {
		return
	}
	tick := time.NewTicker(UpkeepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			u.Sweep(ctx)
		}
	}
}

// Sweep deletes the contexts idle past SessionIdleTTL, then the settled tasks older than TaskRetention. A failure is
// logged, and the next sweep tries again.
func (u *Upkeep) Sweep(ctx context.Context) {
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	if u.SessionIdleTTL > 0 {
		n, err := u.Store.DeleteIdleContexts(ctx, now().Add(-u.SessionIdleTTL))
		switch {
		case err != nil:
			u.Log.Warn("delete idle contexts; will retry", "error", err, "deleted", n)
		case n > 0:
			u.Log.Info("deleted the ADK sessions and tasks of idle contexts", "contexts", n, "session_idle_ttl", u.SessionIdleTTL)
		}
	}
	if u.TaskRetention > 0 {
		n, err := u.Store.DeleteSettledBefore(ctx, now().Add(-u.TaskRetention))
		switch {
		case err != nil:
			u.Log.Warn("delete old settled tasks; will retry", "error", err)
		case n > 0:
			u.Log.Info("deleted old settled tasks", "tasks", n, "task_retention", u.TaskRetention)
		}
	}
}
