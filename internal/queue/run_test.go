package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// joinTimeout bounds every wait on the worker, so a hang fails the test
// instead of stalling it. It is a guard, never a synchronization delay.
const joinTimeout = 5 * time.Second

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(joinTimeout):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// start runs Run on its own goroutine. cancel asks it to stop; wait blocks until
// Run has returned nil and no goroutine it started is left.
func (f *fixture) start(t *testing.T) (cancel context.CancelFunc, wait func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.w.Run(ctx) }()
	return cancel, func() {
		t.Helper()
		if err := recv(t, done, "Run to return"); err != nil {
			t.Errorf("Run = %v; want nil", err)
		}
		if err := goleak.Find(f.ignore); err != nil {
			t.Errorf("goroutines outlived Run: %v", err)
		}
	}
}

func fastPoll(c *Config) { c.PollInterval = time.Millisecond }

func TestRunHandlesQueuedEventsUntilCancelled(t *testing.T) {
	handled := make(chan string, 2)
	f := newFixture(t, func(_ context.Context, e store.Event) error { handled <- e.ID; return nil }, fastPoll)
	f.append(t, "a")
	f.append(t, "b")
	cancel, wait := f.start(t)
	recv(t, handled, "first event")
	recv(t, handled, "second event")
	cancel()
	wait()
	for _, id := range []string{"a", "b"} {
		if got := f.get(t, id); got.Status != store.EventDone {
			t.Errorf("event %s = %+v; want done", id, got)
		}
	}
}

// A handler still running at cancel gets DrainTimeout to finish, and its
// outcome is recorded even though Run's context is already cancelled.
func TestRunDrainLetsInFlightHandlerFinishAndRecordsIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := newFixture(t, func(ctx context.Context, _ store.Event) error {
		started <- struct{}{}
		<-release
		return ctx.Err() // nil unless the drain cancelled the handler
	}, fastPoll, func(c *Config) { c.DrainTimeout = time.Minute })
	f.append(t, "ev")
	cancel, wait := f.start(t)
	recv(t, started, "handler start")
	cancel()
	close(release)
	wait()
	if got := f.get(t, "ev"); got.Status != store.EventDone {
		t.Fatalf("event = %+v; want done", got)
	}
}

// With no drain the handler is cancelled at once; it is not Failed, so its
// lease expires and a later worker redelivers it.
func TestRunZeroDrainAbandonsInFlightHandlerAndLeaseExpiryRedelivers(t *testing.T) {
	started, handled := make(chan struct{}), make(chan int, 1)
	calls := 0
	f := newFixture(t, func(ctx context.Context, e store.Event) error {
		if calls++; calls == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		handled <- e.Attempts
		return nil
	}, fastPoll)
	f.append(t, "ev")
	cancel, wait := f.start(t)
	recv(t, started, "handler start")
	cancel()
	wait()

	got := f.get(t, "ev")
	if got.Status != store.EventProcessing || got.Attempts != 1 || got.LastError != "" {
		t.Fatalf("abandoned event = %+v; want processing at attempt 1, no Fail recorded", got)
	}
	if len(f.logs(t, "WARN", "handler abandoned")) != 1 {
		t.Errorf("abandonment was not logged: %s", f.log)
	}

	f.clock.advance(lease) // the crashed worker's lease expires
	cancel, wait = f.start(t)
	if attempt := recv(t, handled, "redelivery"); attempt != 2 {
		t.Errorf("redelivered at attempt %d; want 2", attempt)
	}
	cancel()
	wait()
	if got = f.get(t, "ev"); got.Status != store.EventDone {
		t.Fatalf("redelivered event = %+v; want done", got)
	}
}

func TestRunSurvivesLostLease(t *testing.T) {
	handled := make(chan struct{})
	var f *fixture
	f = newFixture(t, func(ctx context.Context, e store.Event) error {
		if e.ID == "second" {
			handled <- struct{}{}
			return nil
		}
		// Another worker takes the first event over, then a second event arrives.
		if _, err := f.store.ClaimEvent(ctx, t0.Add(lease), lease); err != nil {
			t.Errorf("reclaim: %v", err)
		}
		f.append(t, "second")
		return nil
	}, fastPoll)
	f.append(t, "first")
	cancel, wait := f.start(t)
	recv(t, handled, "second event after the lost lease")
	cancel()
	wait()
	if got := f.get(t, "first"); got.Status != store.EventProcessing || got.Attempts != 2 {
		t.Fatalf("first = %+v; want the new holder's lease untouched", got)
	}
	if got := f.get(t, "second"); got.Status != store.EventDone {
		t.Fatalf("second = %+v; want done", got)
	}
}

// runCancelledMidHandler starts Run, cancels it while the handler is in
// flight, then lets the handler finish via release and waits for Run to stop.
func (f *fixture) runCancelledMidHandler(t *testing.T, started <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	f.append(t, "ev")
	cancel, wait := f.start(t)
	recv(t, started, "handler start")
	cancel()
	close(release)
	wait()
}

// The abandonment rule covers only handlers cancelled by the drain timeout: a
// handler that fails on its own during the drain window is still Failed.
func TestRunFailsHandlerThatErrorsWithinDrain(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := newFixture(t, func(context.Context, store.Event) error {
		started <- struct{}{}
		<-release
		return errors.New("boom")
	}, fastPoll, func(c *Config) { c.DrainTimeout = time.Minute })
	f.runCancelledMidHandler(t, started, release)

	if got := f.get(t, "ev"); got.Status != store.EventPending || got.Attempts != 1 || got.LastError != "boom" {
		t.Fatalf("event = %+v; want pending at attempt 1 with last error boom", got)
	}
	if n := len(f.logs(t, "WARN", "handler failed")); n != 1 {
		t.Errorf("got %d handler-failed warnings; want 1", n)
	}
	if n := len(f.logs(t, "WARN", "handler abandoned")); n != 0 {
		t.Errorf("a handler that failed on its own was logged as abandoned")
	}
}

// A failed Complete or Fail after shutdown began is the error that must not vanish.
func TestRunLogsOutcomeWriteFailureAfterCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var f *fixture
	f = newFixture(t, func(context.Context, store.Event) error {
		started <- struct{}{}
		<-release // Run's context is cancelled by now
		_ = f.store.Close()
		return nil
	}, fastPoll, func(c *Config) { c.DrainTimeout = time.Minute })
	f.runCancelledMidHandler(t, started, release)

	errs := f.logs(t, "ERROR", "queue worker")
	if len(errs) != 1 || !strings.Contains(errs[0]["error"].(string), "record outcome") {
		t.Fatalf("want one ERROR record with a record-outcome error, got %v", errs)
	}
}

// Only a handler that fails after cancellation is abandoned; one that
// observes the cancellation and returns nil has finished and is completed.
func TestRunCompletesHandlerThatSucceedsAfterCancellation(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, func(ctx context.Context, _ store.Event) error {
		started <- struct{}{}
		<-ctx.Done()
		return nil
	}, fastPoll) // zero drain: the handler's context is cancelled at once
	f.append(t, "ev")
	cancel, wait := f.start(t)
	recv(t, started, "handler start")
	cancel()
	wait()
	if got := f.get(t, "ev"); got.Status != store.EventDone {
		t.Fatalf("event = %+v; want done", got)
	}
}

func TestRunStopsPromptlyWhenIdle(t *testing.T) {
	f := newFixture(t, func(context.Context, store.Event) error { return nil },
		func(c *Config) { c.PollInterval = time.Hour })
	cancel, wait := f.start(t)
	cancel()
	wait() // returns within joinTimeout despite the hour-long poll interval
}
