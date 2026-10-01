package interact_test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// fakeProc is a PressProcessor that does what handle says, and keeps what it was asked.
type fakeProc struct {
	mu     sync.Mutex
	handle func(ctx context.Context, p store.Press) (interact.Result, error)
	calls  []int64 // the Seq of each Process, in the order they began
	gaveUp []int64
	// giveUpErrs are returned by the next GiveUps, one each.
	giveUpErrs []error
}

func (f *fakeProc) Process(ctx context.Context, p store.Press) (interact.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p.Seq)
	f.mu.Unlock()
	return f.handle(ctx, p)
}

func (f *fakeProc) GiveUp(_ context.Context, p store.Press) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gaveUp = append(f.gaveUp, p.Seq)
	if len(f.giveUpErrs) > 0 {
		err := f.giveUpErrs[0]
		f.giveUpErrs = f.giveUpErrs[1:]
		return err
	}
	return nil
}

// processed is how many times each press was handed to Process, by Seq.
func (f *fakeProc) processed() map[int64]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := map[int64]int{}
	for _, s := range f.calls {
		n[s]++
	}
	return n
}

func (f *fakeProc) gaveUpOn() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gaveUp)
}

// pr is a press of an interaction, made at d0.
func pr(seq int64, id string) store.Press {
	return store.Press{Seq: seq, InteractionID: id, User: "UALICE1", Channel: "C1", MessageTS: "100.1", PressedAt: d0}
}

// startWorker runs a Worker, by default on a clock that stands at the time of every press, until the test ends
// or stop is called. It returns what Run returned.
func startWorker(t *testing.T, p interact.PressProcessor, mod func(*interact.WorkerConfig)) (w *interact.Worker, logs *logBuf, stop func() error) {
	t.Helper()
	logs = &logBuf{}
	cfg := interact.WorkerConfig{Processor: p, Now: func() time.Time { return d0 }, Logger: slog.New(slog.NewTextHandler(logs, nil))}
	if mod != nil {
		mod(&cfg)
	}
	w, err := interact.NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	res := make(chan error, 1)
	go func() { res <- w.Run(ctx) }()
	var once sync.Once
	var runErr error
	stop = func() error {
		once.Do(func() { cancel(); runErr = <-res })
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return w, logs, stop
}

// raise lifts *top to n if n is more.
func raise(top *atomic.Int32, n int32) {
	for m := top.Load(); n > m; m = top.Load() {
		if top.CompareAndSwap(m, n) {
			return
		}
	}
}

func TestPressesOfOneInteractionRunOneAtATimeInOrder(t *testing.T) {
	var running, most atomic.Int32
	var mu sync.Mutex
	var order []int64
	f := &fakeProc{handle: func(_ context.Context, p store.Press) (interact.Result, error) {
		raise(&most, running.Add(1))
		time.Sleep(2 * time.Millisecond) // long enough for another worker to join in, if it could
		mu.Lock()
		order = append(order, p.Seq)
		mu.Unlock()
		running.Add(-1)
		return interact.Done, nil
	}}
	w, _, _ := startWorker(t, f, func(c *interact.WorkerConfig) { c.Workers = 4 })
	for seq := int64(1); seq <= 5; seq++ {
		w.Submit(pr(seq, "ix-1"))
	}
	waitFor(t, "five presses", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 5 })
	if most.Load() != 1 || !slices.IsSorted(order) {
		t.Errorf("most at once %d, order %v; want one at a time, oldest first", most.Load(), order)
	}
}

func TestInteractionsRunInParallelUpToTheBound(t *testing.T) {
	var running, most atomic.Int32
	release := make(chan struct{}, 32)
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) {
		raise(&most, running.Add(1))
		<-release
		running.Add(-1)
		return interact.Done, nil
	}}
	w, _, _ := startWorker(t, f, func(c *interact.WorkerConfig) { c.Workers = 8 })
	t.Cleanup(func() { close(release) }) // before the worker is stopped, so that a failure here does not hang
	for seq := int64(1); seq <= 32; seq++ {
		w.Submit(pr(seq, fmt.Sprintf("ix-%d", seq)))
	}
	waitFor(t, "eight running", func() bool { return running.Load() == 8 })
	time.Sleep(20 * time.Millisecond) // a worker beyond the bound would have started by now
	if running.Load() != 8 {
		t.Fatalf("%d running with eight workers", running.Load())
	}
	for range 32 {
		release <- struct{}{}
	}
	waitFor(t, "all 32", func() bool { return len(f.processed()) == 32 && running.Load() == 0 })
	if most.Load() != 8 {
		t.Errorf("most at once %d; want 8: the bound, reached", most.Load())
	}
}

func TestAPressSubmittedAgainWhileItIsInFlightRunsOnce(t *testing.T) {
	gate := make(chan struct{})
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) { <-gate; return interact.Done, nil }}
	w, _, _ := startWorker(t, f, nil)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	w.Submit(pr(1, "ix-1"))
	waitFor(t, "the press", func() bool { return len(f.processed()) == 1 })
	w.Submit(pr(1, "ix-1")) // recovery and the sink can both announce a press
	once.Do(func() { close(gate) })
	w.Submit(pr(2, "ix-1")) // a later press of the same interaction is another press
	waitFor(t, "the second press", func() bool { return f.processed()[2] == 1 })
	if got := f.processed(); got[1] != 1 {
		t.Errorf("processed %v; want press 1 once", got)
	}
}

func TestARetryWaitsAGrowingTimeBeforeTheNext(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if at = append(at, time.Now()); len(at) < 5 {
			return interact.Retry, errBoom
		}
		return interact.Done, nil
	}}
	w, logs, _ := startWorker(t, f, func(c *interact.WorkerConfig) { c.Backoff, c.MaxBackoff = 20*time.Millisecond, 60*time.Millisecond })
	w.Submit(pr(1, "ix-1"))
	waitFor(t, "five tries", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) == 5 })
	for i, want := range []time.Duration{20, 40, 60, 60} { // doubling, to the cap
		if got := at[i+1].Sub(at[i]); got < want*time.Millisecond {
			t.Errorf("try %d came %v after the one before; want at least %v", i+2, got, want*time.Millisecond)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "boom") {
		t.Errorf("log %q; want a warning that names the error", logs.String())
	}
}

// TestAPressWaitingToBeTriedAgainHoldsNoWorker: the wait is a timer, not a sleep on the pool.
func TestAPressWaitingToBeTriedAgainHoldsNoWorker(t *testing.T) {
	f := &fakeProc{handle: func(_ context.Context, p store.Press) (interact.Result, error) {
		if p.InteractionID == "ix-a" {
			return interact.Retry, nil
		}
		return interact.Done, nil
	}}
	w, _, _ := startWorker(t, f, func(c *interact.WorkerConfig) { c.Workers, c.Backoff, c.MaxBackoff = 1, time.Hour, time.Hour })
	w.Submit(pr(1, "ix-a"))
	w.Submit(pr(2, "ix-b"))
	waitFor(t, "the other interaction", func() bool { return f.processed()[2] == 1 })
}

// TestAPressThatKeepsFailingIsGivenUpOn: the cap is the press's age by courier's clock, so a restart
// does not give a press a fresh one, and it is the last word on a press that nothing else ends.
func TestAPressThatKeepsFailingIsGivenUpOn(t *testing.T) {
	var step atomic.Int64 // each try moves the clock on a minute
	f := &fakeProc{handle: func(_ context.Context, p store.Press) (interact.Result, error) {
		if p.Seq == 2 {
			return interact.Done, nil
		}
		step.Add(1)
		return interact.Retry, errBoom
	}}
	w, logs, _ := startWorker(t, f, func(c *interact.WorkerConfig) {
		c.GiveUpAfter, c.Backoff, c.MaxBackoff = 3*time.Minute, time.Millisecond, time.Millisecond
		c.Now = func() time.Time { return d0.Add(time.Duration(step.Load()) * time.Minute) }
	})
	w.Submit(pr(1, "ix-1"))
	waitFor(t, "the press to be given up", func() bool { return len(f.gaveUpOn()) == 1 })
	w.Submit(pr(2, "ix-2")) // the pool goes on, and the press that was given up is not tried again
	waitFor(t, "another press", func() bool { return f.processed()[2] > 0 })
	if got := f.processed()[1]; got != 3 || !slices.Equal(f.gaveUpOn(), []int64{1}) {
		t.Errorf("press 1 tried %d times, given up on %v; want 3 tries, then one give-up", got, f.gaveUpOn())
	}
	if log := logs.String(); !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "interaction=ix-1") || !strings.Contains(log, "boom") {
		t.Errorf("log %q; want an error that names the interaction and the last error", log)
	}
}

func TestAPressThatCannotBeGivenUpOnIsTriedAgain(t *testing.T) {
	f := &fakeProc{giveUpErrs: []error{errBoom}, handle: func(context.Context, store.Press) (interact.Result, error) { return interact.Retry, errBoom }}
	w, _, _ := startWorker(t, f, func(c *interact.WorkerConfig) {
		c.GiveUpAfter, c.Backoff, c.MaxBackoff = time.Minute, time.Millisecond, time.Millisecond
		c.Now = func() time.Time { return d0.Add(time.Hour) }
	})
	w.Submit(pr(1, "ix-1"))
	waitFor(t, "a second give-up", func() bool { return len(f.gaveUpOn()) == 2 })
	if got := f.processed()[1]; got != 2 {
		t.Errorf("tried %d times; want 2: a press whose give-up failed is not lost, and not given up on early", got)
	}
}

// TestAPressOlderThanTheCapStillGetsOneTry: after a long outage the first thing a press gets is a real try.
func TestAPressOlderThanTheCapStillGetsOneTry(t *testing.T) {
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) { return interact.Done, nil }}
	w, _, _ := startWorker(t, f, func(c *interact.WorkerConfig) { c.Now = func() time.Time { return d0.Add(240 * time.Hour) } })
	w.Submit(pr(1, "ix-1"))
	waitFor(t, "the press", func() bool { return f.processed()[1] == 1 })
	if len(f.gaveUpOn()) != 0 {
		t.Errorf("gave up on %v; want a press that is decided never given up on", f.gaveUpOn())
	}
}

// TestShuttingDownLeavesAPressRecorded: a Process cut off by the context is not a failure of the press.
func TestShuttingDownLeavesAPressRecorded(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := &fakeProc{handle: func(ctx context.Context, _ store.Press) (interact.Result, error) {
		if _, ok := ctx.Deadline(); !ok {
			<-ctx.Done()
		}
		return interact.Retry, ctx.Err()
	}}
	w, _, stop := startWorker(t, f, func(c *interact.WorkerConfig) {
		c.Now, c.Backoff, c.MaxBackoff = func() time.Time { return d0.Add(240 * time.Hour) }, time.Hour, time.Hour
	})
	w.Submit(pr(1, "ix-1")) // would be given up on, if it were a failure
	w.Submit(pr(2, "ix-2"))
	waitFor(t, "both presses", func() bool { return len(f.processed()) == 2 })
	if err := stop(); err == nil {
		t.Error("Run returned nil; want the context's error")
	}
	if len(f.gaveUpOn()) != 0 {
		t.Errorf("gave up on %v at shutdown; want the presses left recorded", f.gaveUpOn())
	}
	w.Submit(pr(3, "ix-3")) // after Run: ignored, and recorded for the next start
	goleak.VerifyNone(t, before)
}

// TestPressesWaitingOutABackoffLeaveNoTimerBehind: goleak, with every press waiting an hour.
func TestPressesWaitingOutABackoffLeaveNoTimerBehind(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) { return interact.Retry, nil }}
	w, _, stop := startWorker(t, f, func(c *interact.WorkerConfig) { c.Backoff, c.MaxBackoff = time.Hour, time.Hour })
	for seq := int64(1); seq <= 3; seq++ {
		w.Submit(pr(seq, fmt.Sprintf("ix-%d", seq)))
	}
	waitFor(t, "three tries", func() bool { return len(f.processed()) == 3 })
	_ = stop()
	goleak.VerifyNone(t, before)
}

// stepping is the pipeline, on a clock that each Process moves on half a minute.
type stepping struct {
	*interact.Pipeline
	r *pressRig
}

func (s stepping) Process(ctx context.Context, p store.Press) (interact.Result, error) {
	defer func() { s.r.now = s.r.now.Add(30 * time.Second) }()
	return s.Pipeline.Process(ctx, p)
}

// TestAnEarlyPressIsEndedByItsOwnWindowAndNotMultipliedByTheWorker: the pipeline ends an early press at two
// minutes of its age. The worker's cap is the same kind of limit, on the same clock and the same press time,
// and longer, so a press is never given two windows in a row and never cut short by the worker.
func TestAnEarlyPressIsEndedByItsOwnWindowAndNotMultipliedByTheWorker(t *testing.T) {
	r := newPressRig(t)
	r.pending(t) // the prompt's message is never recorded
	if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	proc := &countingProc{PressProcessor: stepping{r.pl, r}}
	w, _, _ := startWorker(t, proc, func(c *interact.WorkerConfig) {
		c.Backoff, c.MaxBackoff, c.Now = time.Millisecond, time.Millisecond, func() time.Time { return r.now }
	})
	w.Submit(r.got[0])
	waitFor(t, "the press to be ended", func() bool { return r.waiting(t) == 0 })
	if got := r.eph.list(); len(got) != 1 || !strings.Contains(got[0], "not ready yet") || proc.tries.Load() != 5 {
		t.Errorf("told %q after %d tries; want one notice, at the fifth try, which is two minutes in", got, proc.tries.Load())
	}
}

type countingProc struct {
	interact.PressProcessor
	tries atomic.Int32
	mu    sync.Mutex
	at    []time.Time // when each try began
}

func (c *countingProc) Process(ctx context.Context, p store.Press) (interact.Result, error) {
	c.tries.Add(1)
	c.mu.Lock()
	c.at = append(c.at, time.Now())
	c.mu.Unlock()
	return c.PressProcessor.Process(ctx, p)
}

// TestARefusedAnswerOnAParkedTaskIsTriedTwiceAndNoMore answers the question of what bounds a press whose task
// the session holds but does not park: the decider sends a press that is the same person's retry once, and a
// refusal then is final, however often the session refuses. The worker's cap is for the failures that are not so bounded.
func TestARefusedAnswerOnAParkedTaskIsTriedTwiceAndNoMore(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	r.task(t, "task-1") // the session records the task and never parks on it
	r.sess.errs = slices.Repeat([]error{session.ErrNotPaused}, 10)
	if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	proc := &countingProc{PressProcessor: r.pl}
	w, _, _ := startWorker(t, proc, func(c *interact.WorkerConfig) {
		c.Backoff, c.MaxBackoff, c.Now = 30*time.Millisecond, time.Second, func() time.Time { return r.now }
	})
	w.Submit(r.got[0])
	waitFor(t, "the press to be done", func() bool { return r.waiting(t) == 0 })
	if got := len(r.sess.answers()); got != 2 || len(r.eph.list()) != 0 || len(r.out.list()) != 0 {
		t.Errorf("%d sends, told %q, %d notices; want two sends and nothing said: it is not known whether an answer got through", got, r.eph.list(), len(r.out.list()))
	}
	if gap := proc.at[1].Sub(proc.at[0]); gap < 30*time.Millisecond {
		t.Errorf("the second decision came %v after the first; want it paced by the backoff, to give recovery time to park the task", gap)
	}
}

// TestTheFirstBackoffIsSeconds: the default pace of a retry is seconds, not an immediate try.
func TestTheFirstBackoffIsSeconds(t *testing.T) {
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) { return interact.Retry, errBoom }}
	w, _, _ := startWorker(t, f, nil)
	w.Submit(pr(1, "ix-1"))
	waitFor(t, "the first try", func() bool { return f.processed()[1] == 1 })
	time.Sleep(200 * time.Millisecond) // an immediate retry, or a backoff of milliseconds, would have come by now
	if got := f.processed()[1]; got != 1 {
		t.Errorf("%d tries after 200ms; want one: the default first backoff is seconds", got)
	}
}

// TestRecordingAPressNeverWaitsForTheWorker: Notify runs on the path that acknowledges the button to Slack,
// so Worker.Submit, which is what it is given, must return even when every worker is busy and presses queue.
func TestRecordingAPressNeverWaitsForTheWorker(t *testing.T) {
	r := newPressRig(t)
	gate := make(chan struct{})
	f := &fakeProc{handle: func(context.Context, store.Press) (interact.Result, error) { <-gate; return interact.Done, nil }}
	w, _, _ := startWorker(t, f, func(c *interact.WorkerConfig) { c.Workers = 1 })
	t.Cleanup(func() { close(gate) }) // before the worker is stopped
	pl, err := interact.NewPipeline(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud,
		Limits: store.PressLimits{Total: 100, PerUser: 100}, Notify: w.Submit})
	if err != nil {
		t.Fatal(err)
	}
	button := r.submit(t, "UALICE1", 0)
	pressed := make(chan error, 1)
	go func() {
		for range 50 {
			if err := pl.Press(t.Context(), button); err != nil {
				pressed <- err
				return
			}
		}
		pressed <- nil
	}()
	select {
	case err := <-pressed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Press did not return while the only worker was busy")
	}
}
