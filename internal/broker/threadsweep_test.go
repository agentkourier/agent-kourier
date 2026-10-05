package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

var sweepNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// recordingPurger records the cutoffs it is asked to purge at, and fails the first fails calls.
type recordingPurger struct {
	mu    sync.Mutex
	calls []struct{ now, idleBefore time.Time }
	fails int
	n     int
}

func (p *recordingPurger) PurgeTriggerThreads(_ context.Context, now, idleBefore time.Time) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, struct{ now, idleBefore time.Time }{now, idleBefore})
	if p.fails > 0 {
		p.fails--
		return 0, errors.New("disk full")
	}
	return p.n, nil
}

func (p *recordingPurger) waitCalls(t *testing.T, n int) []struct{ now, idleBefore time.Time } {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		p.mu.Lock()
		got := slices.Clone(p.calls)
		p.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep purged %d times; want %d", len(got), n)
		}
	}
}

func runSweep(t *testing.T, p threadPurger, logs io.Writer) (stop func()) {
	t.Helper()
	sw, err := newThreadSweep(threadSweepConfig{Store: p, Interval: time.Millisecond, Now: func() time.Time { return sweepNow }, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run = %v after its context ended; want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the sweep did not stop when its context ended")
		}
	})
	t.Cleanup(stop)
	return stop
}

// The cutoff for an open thread is the longest any trigger's maxIdle can be: a shorter one would delete the row of a
// thread that a trigger with a long maxIdle still counts as live, and the key would start a second investigation.
func TestThreadSweepCutsOffAtTheLongestMaxIdle(t *testing.T) {
	p := &recordingPurger{}
	stop := runSweep(t, p, io.Discard)
	calls := p.waitCalls(t, 2)
	stop()
	for i, c := range calls {
		if !c.now.Equal(sweepNow) || !c.idleBefore.Equal(sweepNow.Add(-v1alpha1.MaxThreadMaxIdle)) {
			t.Errorf("purge %d at %v with idleBefore %v; want now and now - %v", i, c.now, c.idleBefore, v1alpha1.MaxThreadMaxIdle)
		}
	}
}

// A purge that fails is logged without the error's detail beyond its text, and tried again at the next tick.
func TestThreadSweepTriesAgainAfterAFailure(t *testing.T) {
	p := &recordingPurger{fails: 2, n: 3}
	logs := &logBuffer{}
	stop := runSweep(t, p, logs)
	p.waitCalls(t, 4)
	stop()
	if got := logs.String(); !strings.Contains(got, "could not be purged") || !strings.Contains(got, "purged rows") {
		t.Errorf("the log %q; want a warning for the failure and an info line for the rows removed", got)
	}
}

func TestThreadSweepNeedsAStore(t *testing.T) {
	if _, err := newThreadSweep(threadSweepConfig{}); err == nil {
		t.Error("newThreadSweep with no Store succeeded")
	}
}

// Against the real store: the sweep deletes a closed thread past its cooldown and an open one idle longer than any
// maxIdle, and keeps a closed one inside its cooldown and an open one idle for less, which a trigger with a long
// maxIdle (30 days at most) still counts as live.
func TestThreadSweepDeletesOnlyWhatNoTriggerStillCountsLive(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	day := 24 * time.Hour
	// seed makes a started thread opened at opened, last seen then, closed at closed (zero for open), with a cooldown of 4h.
	seed := func(key string, opened, closed time.Time) {
		t.Helper()
		th := store.TriggerThread{Binding: "sre/sre-alerts", KeyHash: key, Thread: "1." + key, Opened: opened}
		if _, ok, err := st.OpenTriggerThread(t.Context(), th, store.TriggerStale{}); err != nil || !ok {
			t.Fatalf("open %s: %v, %v", key, ok, err)
		}
		if err := st.MarkTriggerStarted(t.Context(), th.Binding, key, th.Thread, opened.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if !closed.IsZero() {
			if _, err := st.CloseTriggerThread(t.Context(), th.Binding, key, closed); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("closed-long-ago", sweepNow.Add(-10*day), sweepNow.Add(-10*day+time.Hour))
	seed("closed-in-cooldown", sweepNow.Add(-time.Hour), sweepNow.Add(-30*time.Minute))
	seed("open-idle-31-days", sweepNow.Add(-31*day), time.Time{})
	seed("open-idle-25-days", sweepNow.Add(-25*day), time.Time{})
	seed("open-fresh", sweepNow.Add(-time.Hour), time.Time{})

	stop := runSweep(t, st, io.Discard)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		_, err := st.GetTriggerThread(t.Context(), "sre/sre-alerts", "open-idle-31-days")
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never deleted the thread idle for 31 days")
		}
	}
	stop()
	for key, wantGone := range map[string]bool{"closed-long-ago": true, "open-idle-31-days": true, "closed-in-cooldown": false, "open-idle-25-days": false, "open-fresh": false} {
		_, err := st.GetTriggerThread(t.Context(), "sre/sre-alerts", key)
		if gone := errors.Is(err, store.ErrNotFound); gone != wantGone {
			t.Errorf("%s: gone = %v (%v); want %v", key, gone, err, wantGone)
		}
	}
}

// Run starts the sweep: a thread row the database holds from an earlier run, closed and past its cooldown, is gone
// soon after the broker is up.
func TestRunSweepsTheRowsOfOldTriggerThreads(t *testing.T) {
	c := startConfig(t)
	c.threadSweepEvery = 10 * time.Millisecond
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	th := store.TriggerThread{Binding: "sre/sre-alerts", KeyHash: "old", Thread: "1.1", Opened: old}
	if _, _, err := seed.OpenTriggerThread(t.Context(), th, store.TriggerStale{}); err != nil {
		t.Fatal(err)
	}
	if err := seed.MarkTriggerStarted(t.Context(), th.Binding, th.KeyHash, th.Thread, old.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CloseTriggerThread(t.Context(), th.Binding, th.KeyHash, old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	r := start(t, c)
	r.until("/readyz", 200)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // the store's own metric says the sweep ran
		if _, body := r.get("/metrics"); strings.Contains(body, `operation="courier.store.PurgeTriggerThreads"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never ran")
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.GetTriggerThread(t.Context(), th.Binding, th.KeyHash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old closed thread after a run: %v; want it swept", err)
	}
}
