package queue

import (
	"math/rand/v2"
	"time"

	"github.com/agentkourier/agent-kourier/internal/backoff"
)

// Default retry backoff bounds, used when Config.Backoff is nil.
const (
	defaultBackoffBase    = time.Second
	defaultBackoffCeiling = 5 * time.Minute
)

// ExponentialBackoff returns a backoff function for Config.Backoff. The delay
// before retrying after the nth failed attempt is base*2^(n-1), capped at
// ceiling, then jittered uniformly into [d/2, d] so that events that failed
// together do not retry together. Attempts below 1 count as the first, and a
// non-positive base yields no delay.
func ExponentialBackoff(base, ceiling time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		d := backoff.Doubling(base, ceiling, attempt)
		if d <= 0 {
			return 0
		}
		return d/2 + rand.N(d/2+1) //nolint:gosec // retry jitter, not security
	}
}
