package runtime

import (
	"context"
	"log/slog"
	"time"
)

// upkeep keeps the leader's store from growing without bound: it releases the contexts idle past SESSION_IDLE_TTL, and
// deletes the settled tasks older than TASK_RETENTION.
type upkeep struct {
	store     Store
	releaser  ContextReleaser // nil: the store deletes idle contexts in its own transaction
	reporter  IdleReporter    // nil: told nothing
	retention time.Duration   // zero keeps settled tasks
	idle      time.Duration   // zero keeps contexts
	every     time.Duration
	now       func() time.Time
	log       *slog.Logger
}

// run sweeps every u.every until ctx ends. It returns at once when both durations are zero.
func (u *upkeep) run(ctx context.Context) {
	if u.retention <= 0 && u.idle <= 0 {
		return
	}
	tick := time.NewTicker(u.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if ctx.Err() == nil { // a tick and the end can come together
				u.sweep(ctx)
			}
		}
	}
}

// sweep releases the idle contexts, then deletes the old settled tasks. A failure is logged, and the next sweep tries
// again.
func (u *upkeep) sweep(ctx context.Context) {
	if u.idle > 0 {
		cutoff := u.now().Add(-u.idle)
		if u.releaser != nil {
			u.release(ctx, cutoff)
		} else {
			n, err := u.store.DeleteIdleContexts(ctx, cutoff)
			if u.reporter != nil {
				u.reporter.IdleContextsDeleted(n, err)
			}
			if err != nil {
				u.log.Warn("delete idle contexts; will retry", "error", err, "deleted", n)
			}
		}
	}
	if u.retention > 0 {
		n, err := u.store.DeleteSettledBefore(ctx, u.now().Add(-u.retention))
		switch {
		case err != nil:
			u.log.Warn("delete old settled tasks; will retry", "error", err)
		case n > 0:
			u.log.Info("deleted old settled tasks", "tasks", n, "task_retention", u.retention)
		}
	}
}

// release has the backend let go of each idle context with no turn running, then forgets the context and its tasks
// from before the release: they belong to what the backend let go of and can never run again. A context the backend
// could not release is left for the next sweep.
func (u *upkeep) release(ctx context.Context, cutoff time.Time) {
	idle, err := u.store.IdleContexts(ctx, cutoff)
	if err != nil {
		u.log.Warn("find idle contexts; will retry", "error", err)
		return
	}
	for _, c := range idle {
		if ctx.Err() != nil {
			return
		}
		at := u.now()
		if err := u.releaser.ReleaseContext(ctx, c); err != nil {
			u.log.Warn("release an idle context; will retry", "context", c.ContextID, "error", err)
			continue
		}
		if err := u.store.ForgetContext(ctx, c.Owner, c.ContextID, c.Ref, at); err != nil {
			u.log.Warn("forget a released context", "context", c.ContextID, "error", err)
		}
	}
}
