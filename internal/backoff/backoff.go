// Package backoff holds the doubling wait that the retry loops of the broker share. It is a leaf: it imports
// nothing of the module, so any package may use it.
package backoff

import "time"

// Doubling is the wait after tries failures in a row: base for the first, twice that for the second, and so
// on, never more than ceiling. A tries below 1 counts as the first. A base above ceiling gives ceiling, and a
// base of zero or less never grows. The doubling stops at ceiling, so a long run of failures cannot overflow.
func Doubling(base, ceiling time.Duration, tries int) time.Duration {
	d := base
	for i := 1; i < tries && d > 0 && d < ceiling; i++ {
		if d > ceiling/2 { // doubling would pass the cap (and could overflow)
			d = ceiling
		} else {
			d *= 2
		}
	}
	return min(d, ceiling)
}
