package queue

import (
	"math/rand/v2"
	"time"
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
		d := min(base, ceiling)
		for ; attempt > 1 && d > 0 && d < ceiling; attempt-- {
			if d > ceiling/2 { // doubling would pass the cap (and could overflow)
				d = ceiling
			} else {
				d *= 2
			}
		}
		if d <= 0 {
			return 0
		}
		return d/2 + rand.N(d/2+1) //nolint:gosec // retry jitter, not security
	}
}
