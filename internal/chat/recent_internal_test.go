package chat

import (
	"strconv"
	"testing"
)

// TestRecentIsBounded checks both halves of the dedup set's bound: a key survives the
// generation after the one it was added in, and is forgotten after that, and the set never
// holds more than two generations.
func TestRecentIsBounded(t *testing.T) {
	var r recent
	add := func(i int) {
		t.Helper()
		r.add([2]string{"C1", strconv.Itoa(i)})
		if n := len(r.cur) + len(r.old); n > 2*recentMax {
			t.Fatalf("recent holds %d keys; the bound is %d", n, 2*recentMax)
		}
	}
	for i := 0; i <= recentMax; i++ { // one more than a generation holds
		add(i)
	}
	if !r.has([2]string{"C1", "0"}) {
		t.Fatal("the first key was forgotten when its generation filled; it should live in the older one")
	}
	for i := recentMax + 1; i <= 2*recentMax; i++ {
		add(i)
	}
	if r.has([2]string{"C1", "0"}) {
		t.Fatal("the first key is still remembered after two generations")
	}
}
