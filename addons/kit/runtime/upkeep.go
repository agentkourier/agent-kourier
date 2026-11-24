package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/store"
)

// upkeep keeps the leader's store from growing without bound: it releases the contexts idle past SESSION_IDLE_TTL, and
// deletes the settled tasks older than TASK_RETENTION.
type upkeep struct {
	store     Store
	releaser  ContextReleaser  // nil: the store deletes idle contexts in its own transaction
	contexts  *keyLock[string] // by context ID: held while a context is released, and by the accept of a message (touch)
	reporter  IdleReporter     // nil: told nothing
	retention time.Duration    // zero keeps settled tasks
	idle      time.Duration    // zero keeps contexts
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
// from before the release: they belong to what the backend let go of and can never run again. A context active since
// it was listed, one whose turn the backend found begun (ErrContextBusy), and one the backend could not release are
// left for the next sweep.
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
		u.releaseOne(ctx, c)
	}
}

// releaseOne releases c and forgets it, under the context's lock, which the accept of a message for the context takes
// for its touch, so that no message is accepted for it from the check to the forget. A message accepted since the
// listing moved the context's last activity, and the check (StillIdle) leaves the context for a later sweep; one that
// arrives during the release waits for it, and its touch records the context anew. The release is cut off after
// ContextReleaseTimeout, so that it holds such a message back no longer than that. The forget is not: it is one write
// of the store, which the store's own timeouts bound, and cutting it after a release that succeeded would leave the
// store naming what the backend let go of.
func (u *upkeep) releaseOne(ctx context.Context, c store.Context) {
	unlock, err := u.contexts.lock(ctx, c.ContextID)
	if err != nil {
		return // the upkeep is stopping
	}
	defer unlock()
	still, err := u.store.StillIdle(ctx, c)
	switch {
	case err != nil:
		u.log.Warn("check an idle context; will retry", "context", c.ContextID, "error", err)
		return
	case !still:
		u.log.Debug("an idle context has been active since the sweep listed it; left for a later sweep", "context", c.ContextID)
		return
	}
	at := u.now()
	releaseCtx, cancel := context.WithTimeout(ctx, ContextReleaseTimeout)
	defer cancel()
	if err := u.releaser.ReleaseContext(releaseCtx, c); err != nil {
		if errors.Is(err, ErrContextBusy) {
			u.log.Debug("an idle context's turn has begun; left for a later sweep", "context", c.ContextID)
			return
		}
		u.log.Warn("release an idle context; will retry", "context", c.ContextID, "error", err)
		return
	}
	if err := u.store.ForgetContext(ctx, c.Owner, c.ContextID, c.Ref, at); err != nil {
		u.log.Warn("forget a released context", "context", c.ContextID, "error", err)
	}
}
