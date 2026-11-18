package teams

import (
	"sync"
	"time"
)

// The bounds of the dedup by activity ID. Teams sends a message again only after a fast error answer, once (measured on a test tenant
// on 2026-11-18, for a message only), and the receiver releases the key of an activity it does not take, so that delivery is taken. The dedup guards
// against a request that comes twice all the same (a replay of a captured request while its token is valid, or a proxy that retries),
// and is the place where the Graph intake will meet the activities.
//
// It is the second set of keys in two generations, after the chat intake's (internal/trigger/intake.go); a shared package replaces
// both, as internal/chat/lanes did the lanes.
const (
	// dedupWindow is how long an activity is remembered: at least this long, and less than twice it.
	dedupWindow = 10 * time.Minute
	// dedupMax caps the activities one generation of the set holds; the set holds two. A generation that is full is set aside early, so
	// under a flood an activity may be remembered for less than the window.
	dedupMax = 4096
)

// recent is a set of keys in two generations: cur takes new keys, and old is the generation before, which is dropped when cur is set
// aside, once it holds dedupMax keys or its window has passed. The windows follow each other from the first claim, whenever the claims
// come, so a key is dropped at most two windows after its own began. It is safe for concurrent use.
type recent struct {
	now func() time.Time

	mu       sync.Mutex
	cur, old map[string]struct{}
	since    time.Time // when cur's window began
}

func newRecent(now func() time.Time) *recent { return &recent{now: now} }

// claim records k and says whether it was new.
func (r *recent) claim(k string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	switch age := now.Sub(r.since); {
	case r.cur == nil || age >= 2*dedupWindow:
		r.old, r.cur, r.since = nil, map[string]struct{}{}, now
	case age >= dedupWindow:
		r.old, r.cur, r.since = r.cur, map[string]struct{}{}, r.since.Add(dedupWindow)
	case len(r.cur) >= dedupMax:
		r.old, r.cur, r.since = r.cur, map[string]struct{}{}, now
	}
	_, inCur := r.cur[k]
	_, inOld := r.old[k]
	if inCur || inOld {
		return false
	}
	r.cur[k] = struct{}{}
	return true
}

// forget removes k, so that it is new again: for an activity that was claimed and then not taken.
func (r *recent) forget(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cur, k)
	delete(r.old, k)
}
