package queue

import (
	"testing"
	"time"
)

func TestExponentialBackoffGrowsCapsAndJitters(t *testing.T) {
	const base, ceiling = time.Second, 10 * time.Second
	b := ExponentialBackoff(base, ceiling)
	for attempt, ceil := range map[int]time.Duration{
		0: time.Second, // treated as the first attempt
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: ceiling,
		6: ceiling,
		// A huge attempt count must neither overflow nor exceed the cap.
		1 << 30: ceiling,
	} {
		varied := map[time.Duration]bool{}
		for range 200 {
			d := b(attempt)
			if d < ceil/2 || d > ceil {
				t.Fatalf("backoff(%d) = %v; want within [%v, %v]", attempt, d, ceil/2, ceil)
			}
			varied[d] = true
		}
		if len(varied) < 2 {
			t.Errorf("backoff(%d) never varied; jitter is missing", attempt)
		}
	}
}

func TestExponentialBackoffNonPositiveBaseIsZero(t *testing.T) {
	if d := ExponentialBackoff(0, time.Minute)(3); d != 0 {
		t.Fatalf("backoff with zero base = %v; want 0", d)
	}
}
