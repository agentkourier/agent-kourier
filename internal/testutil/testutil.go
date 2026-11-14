// Package testutil holds the small helpers that tests across the repository share: waiting for a condition, and a
// logger that drops what it is given. It is imported only from _test.go files and from test support under test/;
// nothing the product runs uses it. It imports the standard library alone, so that any test can use it without an
// import cycle.
package testutil

import (
	"log/slog"
	"testing"
	"time"
)

// DefaultTimeout is how long Eventually waits when no Within is given. It is only how long a condition that will never
// hold is waited for before the test fails, and is long: a loaded machine can starve the goroutine that makes it hold.
const DefaultTimeout = 30 * time.Second

// DefaultInterval is how long Eventually sleeps between two checks when no Every is given.
const DefaultInterval = time.Millisecond

// Option changes how Eventually polls.
type Option func(*poll)

type poll struct {
	timeout, interval time.Duration
}

// Within sets how long Eventually waits for the condition before it fails the test, instead of DefaultTimeout.
func Within(d time.Duration) Option { return func(p *poll) { p.timeout = d } }

// Every sets how long Eventually sleeps between two checks of the condition, instead of DefaultInterval.
func Every(d time.Duration) Option { return func(p *poll) { p.interval = d } }

// Eventually checks cond until it returns true, sleeping the interval between two checks. If the timeout passes first,
// it fails the test with t.Fatalf, naming what and the timeout. The deadline is looked at only after a check of cond has
// failed, so a condition that holds by the last check passes.
func Eventually(t testing.TB, what string, cond func() bool, opts ...Option) {
	t.Helper()
	p := poll{timeout: DefaultTimeout, interval: DefaultInterval}
	for _, o := range opts {
		o(&p)
	}
	for deadline := time.Now().Add(p.timeout); !cond(); time.Sleep(p.interval) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %s", p.timeout, what)
			return // for a testing.TB whose Fatalf does not end the goroutine
		}
	}
}

// DiscardLogger returns a logger that drops every record: it is enabled at no level, so it formats nothing.
func DiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
