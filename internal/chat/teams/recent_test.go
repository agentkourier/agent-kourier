package teams

import (
	"fmt"
	"testing"
	"time"
)

// TestRecentRemembersForTheWindow: a key is a duplicate for at least dedupWindow and at most twice it, and a forgotten key is new again.
func TestRecentRemembersForTheWindow(t *testing.T) {
	clock := newFakeClock()
	r := newRecent(clock.now)
	if !r.claim("a") {
		t.Fatal("a first key is not new")
	}
	if r.claim("a") {
		t.Fatal("a repeated key is new")
	}
	clock.advance(dedupWindow - time.Second)
	if r.claim("a") {
		t.Fatal("a key within the window is new")
	}
	clock.advance(time.Second)
	if r.claim("a") {
		t.Fatal("a key one window old is new: it must last at least the window")
	}
	clock.advance(2 * dedupWindow)
	if !r.claim("a") {
		t.Fatal("a key two windows old is still a duplicate")
	}
	r.forget("a")
	if !r.claim("a") {
		t.Fatal("a forgotten key is not new")
	}
}

// TestRecentForgetsWithinTwoWindows: a key is dropped two windows after the window it was claimed in began, however the later claims
// fall, and not a window later because a claim came just before the end of the second.
func TestRecentForgetsWithinTwoWindows(t *testing.T) {
	clock := newFakeClock()
	r := newRecent(clock.now)
	r.claim("a")
	clock.advance(2*dedupWindow - time.Second)
	if r.claim("a") {
		t.Fatal("a key is new just before two windows have passed")
	}
	clock.advance(time.Second)
	if !r.claim("a") {
		t.Fatal("a key is still a duplicate two windows after its window began")
	}
}

// TestRecentHoldsAtMostTwoGenerations: a generation that reaches dedupMax keys is set aside, so the set holds at most twice dedupMax,
// and the keys of the generation before are forgotten.
func TestRecentHoldsAtMostTwoGenerations(t *testing.T) {
	clock := newFakeClock()
	r := newRecent(clock.now)
	for i := range 2 * dedupMax {
		r.claim(fmt.Sprint(i))
	}
	if !r.claim("0") {
		t.Fatal("the first key is still kept after two full generations")
	}
	if got := len(r.cur) + len(r.old); got > 2*dedupMax {
		t.Fatalf("the set holds %d keys, more than %d", got, 2*dedupMax)
	}
	if r.claim(fmt.Sprint(2*dedupMax - 1)) {
		t.Fatal("the last key claimed is new")
	}
}
