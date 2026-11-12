package testutil_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// fakeTB records what a test would be failed with, and fails nothing: Eventually returns after Fatalf, so a fake whose
// Fatalf does not end the goroutine still ends the wait.
type fakeTB struct {
	testing.TB
	helpers int
	fatal   []string
}

func (f *fakeTB) Helper() { f.helpers++ }

func (f *fakeTB) Fatalf(format string, args ...any) { f.fatal = append(f.fatal, fmt.Sprintf(format, args...)) }

func TestEventuallyReturnsOnceTheConditionHolds(t *testing.T) {
	f := &fakeTB{TB: t}
	var checks atomic.Int32
	testutil.Eventually(f, "the third check", func() bool { return checks.Add(1) >= 3 }, testutil.Within(time.Minute))
	if len(f.fatal) != 0 {
		t.Fatalf("Eventually failed the test with %q; want it to pass once the condition holds", f.fatal)
	}
	if n := checks.Load(); n != 3 {
		t.Errorf("the condition was checked %d times; want 3, no check after it held", n)
	}
	if f.helpers == 0 {
		t.Error("Eventually did not mark itself a helper")
	}
}

func TestEventuallyChecksOnceWhenTheConditionHoldsAtOnce(t *testing.T) {
	f := &fakeTB{TB: t}
	checks := 0
	testutil.Eventually(f, "nothing", func() bool { checks++; return true })
	if len(f.fatal) != 0 || checks != 1 {
		t.Errorf("fatal %q after %d checks; want no failure after one", f.fatal, checks)
	}
}

func TestEventuallyFailsNamingWhatAndTheTimeout(t *testing.T) {
	f := &fakeTB{TB: t}
	start := time.Now()
	testutil.Eventually(f, "the impossible", func() bool { return false },
		testutil.Within(50*time.Millisecond), testutil.Every(5*time.Millisecond))
	if took := time.Since(start); took < 50*time.Millisecond {
		t.Errorf("Eventually gave up after %s; want it to wait the whole 50ms", took)
	}
	if len(f.fatal) != 1 {
		t.Fatalf("Eventually failed the test %d times (%q); want once", len(f.fatal), f.fatal)
	}
	if got := f.fatal[0]; !strings.Contains(got, "the impossible") || !strings.Contains(got, "50ms") {
		t.Errorf("the failure says %q; want it to name what was waited for and for how long", got)
	}
}

// The interval is the sleep between two checks: a check that never holds within a timeout of 100ms, with an interval of
// 40ms, is made a few times and not hundreds.
func TestEventuallySleepsTheIntervalBetweenChecks(t *testing.T) {
	f := &fakeTB{TB: t}
	var checks atomic.Int32
	testutil.Eventually(f, "never", func() bool { checks.Add(1); return false },
		testutil.Within(100*time.Millisecond), testutil.Every(40*time.Millisecond))
	if n := checks.Load(); n < 2 || n > 5 {
		t.Errorf("the condition was checked %d times in 100ms at an interval of 40ms; want 2 to 5", n)
	}
}

// The defaults are a long timeout, which is only how long a condition that never holds is waited for, and a short
// interval.
func TestEventuallyDefaults(t *testing.T) {
	if testutil.DefaultTimeout != 30*time.Second || testutil.DefaultInterval != time.Millisecond {
		t.Errorf("defaults = %s, %s; want 30s, 1ms", testutil.DefaultTimeout, testutil.DefaultInterval)
	}
}

func TestDiscardLoggerDiscardsEverything(t *testing.T) {
	log := testutil.DiscardLogger()
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if log.Enabled(context.Background(), level) {
			t.Errorf("the logger is enabled at %v; want it to drop every record", level)
		}
	}
}
