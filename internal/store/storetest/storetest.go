// Package storetest holds what a store.Store implementation is held to: one suite of behaviour tests
// over the interfaces of package store, written in domain terms and never in SQL, so that every
// implementation (SQLite, and Postgres after it) answers to the same promises. Run is the entry
// point; an implementation's own tests call it with a function that opens a fresh, empty store.
//
// The suite pins what the store does today where the doc comments of package store are silent (the
// order of ties, the precision of times, which errors a failed claim returns). Each such case is
// named in its test, so that an implementation that differs sees which promise it breaks.
//
// Implementation-specific tests (migrations, pragmas, query plans, a database trigger, file
// handling) stay with the implementation: they say how a store keeps its promises, not what they are.
package storetest

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// Instance is a store under test and, if it has one, a way to close it and open it again.
type Instance struct {
	// Store is a fresh store with nothing in it. The opener registers its cleanup (t.Cleanup); Close
	// may be called on a store more than once without error.
	Store store.Store
	// Reopen closes Store and returns a new store over the same data, as a restart does. It may be
	// nil, which skips the tests that need a restart. It may be called more than once: each call
	// closes the store the previous call returned.
	Reopen func(t *testing.T) store.Store
}

// Run runs the whole suite as subtests of t. open is called once per subtest and returns a store
// that is empty and that no other subtest uses. The subtests are named for the interface they test,
// so a failing implementation can run one part with -run.
func Run(t *testing.T, open func(t *testing.T) Instance) {
	t.Helper()
	su := suite{open: open}
	for _, g := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"Events", su.events},
		{"Queue", su.queue},
		{"Sessions", su.sessions},
		{"SessionTasks", su.sessionTasks},
		{"Recovery", su.recovery},
		{"ReplyQueue", su.replyQueue},
		{"Outbox", su.outbox},
		{"Interactions", su.interactions},
		{"Presses", su.presses},
		{"Audit", su.audit},
		{"TriggerThreads", su.triggerThreads},
		{"TriggerRuns", su.triggerRuns},
		{"Fidelity", su.fidelity},
		{"TraceContext", su.traceContext},
		{"Lifecycle", su.lifecycle},
	} {
		t.Run(g.name, g.fn)
	}
}

type suite struct {
	open func(t *testing.T) Instance
}

// run runs fn as a subtest named name, on a fresh store.
func (su suite) run(t *testing.T, name string, fn func(t *testing.T, s store.Store)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		fn(t, su.fresh(t).Store)
	})
}

// runReopen is run for a test that restarts the store. reopen returns the new store.
func (su suite) runReopen(t *testing.T, name string, fn func(t *testing.T, s store.Store, reopen func() store.Store)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		in := su.fresh(t)
		if in.Reopen == nil {
			t.Skip("the store under test cannot be reopened")
		}
		fn(t, in.Store, func() store.Store {
			t.Helper()
			s := in.Reopen(t)
			if s == nil {
				t.Fatal("storetest: Reopen returned no store")
			}
			return s
		})
	})
}

func (su suite) fresh(t *testing.T) Instance {
	t.Helper()
	in := su.open(t)
	if in.Store == nil {
		t.Fatal("storetest: open returned no store")
	}
	return in
}

var (
	// t0 is the clock the tests start from, in UTC.
	t0  = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	key = store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	ttl = 90 * 24 * time.Hour
)

func keyOf(thread string) store.ConversationKey {
	return store.ConversationKey{Connection: "slack", Channel: "C1", Thread: thread}
}

// isErr reports a failure, and lets the test go on, if err is not want.
func isErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s = %v; want %v", what, err, want)
	}
}

// inTieGroups reports whether got is groups one after another, each group in any order: rows that tie on what
// a list is ordered by come in no particular order, and nothing of courier's reads them in one.
func inTieGroups(got []string, groups ...[]string) bool {
	for _, g := range groups {
		if len(got) < len(g) {
			return false
		}
		head := slices.Clone(got[:len(g)])
		want := slices.Clone(g)
		slices.Sort(head)
		slices.Sort(want)
		if !slices.Equal(head, want) {
			return false
		}
		got = got[len(g):]
	}
	return len(got) == 0
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// noErr fails the test at once if err is not nil.
func noErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// parallel runs fn(0) to fn(n-1) on n goroutines that are released together, and waits for them. fn
// must report failures with t.Errorf, never t.Fatal.
func parallel(n int, fn func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			fn(i)
		})
	}
	close(start)
	wg.Wait()
}
