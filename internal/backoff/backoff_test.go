package backoff_test

import (
	"math"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/backoff"
)

func TestDoubling(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name  string
		base  time.Duration
		max   time.Duration
		tries int
		want  time.Duration
	}{
		{"first try waits the base", s, 30 * s, 1, s},
		{"second doubles", s, 30 * s, 2, 2 * s},
		{"fifth", s, 30 * s, 5, 16 * s},
		{"capped at max", s, 30 * s, 6, 30 * s},
		{"far past the cap", s, 30 * s, 1000, 30 * s},
		{"tries below one count as the first", s, 30 * s, 0, s},
		{"negative tries count as the first", s, 30 * s, -3, s},
		{"base above max is cut to max", time.Minute, 30 * s, 1, 30 * s},
		{"base equals max", 5 * s, 5 * s, 4, 5 * s},
		{"zero base yields no delay", 0, 30 * s, 4, 0},
		{"doubling would overflow", time.Duration(math.MaxInt64/2 + 1), time.Duration(math.MaxInt64), 5, time.Duration(math.MaxInt64)},
		{"max near the limit", s, time.Duration(math.MaxInt64), 70, time.Duration(math.MaxInt64)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := backoff.Doubling(tc.base, tc.max, tc.tries); got != tc.want {
				t.Fatalf("Doubling(%v, %v, %d) = %v, want %v", tc.base, tc.max, tc.tries, got, tc.want)
			}
		})
	}
}
