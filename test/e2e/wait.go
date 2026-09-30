package e2e

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// pollInterval is how often Wait re-checks a condition.
const pollInterval = 50 * time.Millisecond

// ErrTimeout is wrapped by the error Wait returns when the deadline passes first.
var ErrTimeout = errors.New("e2e: timed out")

// Wait calls cond at once and then every pollInterval until it reports true, and gives up after
// timeout. It never sleeps a fixed time: it ends when the condition holds. An error from cond does
// not end the wait (a restarting service refuses connections for a while); the last one is wrapped
// in the timeout error, so a broken dependency is not mistaken for a quiet one. (true, err) is not
// success. A cancelled ctx ends the wait with ctx's error, not ErrTimeout.
func Wait(ctx context.Context, timeout time.Duration, desc string, cond func(context.Context) (bool, error)) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		ok, err := cond(wctx)
		if ok && err == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			switch {
			case ctx.Err() != nil:
				return fmt.Errorf("waiting for %s: %w", desc, ctx.Err())
			case err != nil:
				return fmt.Errorf("%w after %s waiting for %s: last error: %w", ErrTimeout, timeout, desc, err)
			}
			return fmt.Errorf("%w after %s waiting for %s", ErrTimeout, timeout, desc)
		case <-tick.C:
		}
	}
}
