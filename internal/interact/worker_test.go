package interact_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/audit"
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

// fakeSource lists the presses it is given, or the error it is given.
type fakeSource struct {
	presses []store.Press
	err     error
}

func (f fakeSource) ListPresses(context.Context) ([]store.Press, error) { return f.presses, f.err }

func TestRecoverSubmitsEveryRecordedPressOnceAndInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []int64
	gate := make(chan struct{}) // holds press 2 in flight while it is listed
	f := &fakeProc{handle: func(_ context.Context, p store.Press) (interact.Result, error) {
		<-gate
		mu.Lock()
		defer mu.Unlock()
		order = append(order, p.Seq)
		return interact.Done, nil
	}}
	w, _, _ := startWorker(t, f, nil)
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)      // before the worker is stopped
	w.Submit(pr(2, "ix-1")) // announced by the sink as well as listed: it still runs once
	waitFor(t, "press 2 to be in flight", func() bool { return f.processed()[2] == 1 })
	src := fakeSource{presses: []store.Press{pr(1, "ix-1"), pr(2, "ix-1"), pr(3, "ix-1")}}
	if err := w.Recover(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	release()
	waitFor(t, "three presses", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 3 })
	if got := f.processed(); got[1] != 1 || got[2] != 1 || got[3] != 1 || !slices.Equal(order[1:], []int64{1, 3}) {
		t.Errorf("processed %v, in order %v; want each press once, the listed ones oldest first", got, order)
	}
	if err := w.Recover(t.Context(), fakeSource{err: errBoom}); !errors.Is(err, errBoom) {
		t.Errorf("Recover with a store that fails = %v; want its error", err)
	}
}

// crashRig is a press rig whose worker can be killed and started again over the same store, as a restart is.
type crashRig struct {
	*pressRig
}

// restart starts a worker over the store, recovers its presses and returns the stop that ends it.
func (c crashRig) restart(t *testing.T, mod func(*interact.WorkerConfig)) (stop func() error) {
	t.Helper()
	w, _, stop := startWorker(t, c.pl, func(cfg *interact.WorkerConfig) {
		cfg.Now = func() time.Time { return c.now }
		if mod != nil {
			mod(cfg)
		}
	})
	if err := w.Recover(t.Context(), c.st); err != nil {
		t.Fatal(err)
	}
	return stop
}

// TestAPressThatCannotBeCheckedIsEndedByTheWorkersCapAlone: the users API is down past the deadline. The press
// is not ended at 2 minutes with a message that the check failed: it waits for the worker's cap, and is
// then given up on, which tells the person to press again.
func TestAPressThatCannotBeCheckedIsEndedByTheWorkersCapAlone(t *testing.T) {
	c := crashRig{newPressRig(t)}
	c.open(t)
	c.az.memberErr = errBoom
	if err := c.pl.Press(t.Context(), c.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(61 * time.Minute) // past the cap, and the deadline: before the restart, so no goroutine races the clock
	c.restart(t, nil)
	waitFor(t, "the press to be given up on", func() bool { return c.waiting(t) == 0 })
	want := "UALICE1: Couldn't process your answer. Please press Submit again."
	if got := strings.Join(c.eph.list(), "|"); got != want || c.row(t).State != store.InteractionPending || len(c.aud.got) != 0 {
		t.Errorf("told %q, state %s, %d audit rows; want the person told to press again, the question untouched and nothing audited", got, c.row(t).State, len(c.aud.got))
	}
}

// TestAPressRecordedBeforeACrashIsDecidedAfterIt: the crash is after the record and before any work.
func TestAPressRecordedBeforeACrashIsDecidedAfterIt(t *testing.T) {
	c := crashRig{newPressRig(t)}
	c.open(t)
	if err := c.pl.Press(t.Context(), c.submit(t, "UALICE1", 1)); err != nil {
		t.Fatal(err)
	}
	if c.waiting(t) != 1 || c.row(t).State != store.InteractionPending {
		t.Fatalf("%d presses waiting, state %s; want the press recorded and nothing decided", c.waiting(t), c.row(t).State)
	}
	c.restart(t, nil)
	waitFor(t, "the press to be decided", func() bool { return c.waiting(t) == 0 })
	if row := c.row(t); row.State != store.InteractionAnswered || row.Decider != "UALICE1" || len(c.sess.answers()) != 1 || len(c.eph.list()) != 0 {
		t.Errorf("state %s by %q, %d sends, told %q; want answered by the presser, sent once, no one told", row.State, row.Decider, len(c.sess.answers()), c.eph.list())
	}
}

// TestACrashAfterTheDecisionIsRecordedSendsTheAnswerAgain: the press found its answer recorded by the same
// person, and the decider sends it again, as the same message, without a second decision.
func TestACrashAfterTheDecisionIsRecordedSendsTheAnswerAgain(t *testing.T) {
	c := crashRig{newPressRig(t)} // its store keeps goroutines of its own until the test ends
	before := goleak.IgnoreCurrent()
	c.open(t)
	c.sess.errs = []error{errBoom} // the send fails, as a crash right after the decision would leave it
	if err := c.pl.Press(t.Context(), c.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	stop := c.restart(t, func(cfg *interact.WorkerConfig) { cfg.Backoff, cfg.MaxBackoff = time.Hour, time.Hour })
	waitFor(t, "the decision and its failed send", func() bool { return len(c.sess.answers()) == 1 })
	_ = stop() // the crash
	if c.row(t).State != store.InteractionAnswered || c.waiting(t) != 1 {
		t.Fatalf("state %s, %d presses waiting; want the answer recorded and the press kept", c.row(t).State, c.waiting(t))
	}
	stop = c.restart(t, nil)
	waitFor(t, "the press to be decided again", func() bool { return c.waiting(t) == 0 })
	sent := c.sess.answers()
	if len(sent) != 2 || sent[0].ID != sent[1].ID || len(c.aud.got) != 1 || len(c.eph.list()) != 0 || c.row(t).Decider != "UALICE1" {
		t.Errorf("%d sends, %d audit rows, told %q; want the same message sent twice, one decision, and no one told they lost", len(sent), len(c.aud.got), c.eph.list())
	}
	_ = stop()
	goleak.VerifyNone(t, before)
}

// TestPressesRecordedTwiceAreDecidedOnceInOrder: a person pressing twice, then another: one decision, the
// same answer sent for the repeat, and the other told who was first, which is no decision to audit.
func TestPressesRecordedTwiceAreDecidedOnceInOrder(t *testing.T) {
	c := crashRig{newPressRig(t)}
	c.open(t)
	for _, user := range []string{"UALICE1", "UALICE1", "UBOB22"} {
		if err := c.pl.Press(t.Context(), c.submit(t, user, 0)); err != nil {
			t.Fatal(err)
		}
	}
	c.restart(t, nil)
	waitFor(t, "all three presses", func() bool { return c.waiting(t) == 0 })
	want := []string{"UBOB22: <@UALICE1> answered this question first."}
	if got := c.eph.list(); !slices.Equal(got, want) || len(c.sess.answers()) != 2 || len(c.aud.got) != 1 || c.aud.got[0].Result != audit.DecisionAnswered {
		t.Errorf("told %q, %d sends, audit %+v; want the repeat sent again, the other told, and the one decision audited", got, len(c.sess.answers()), c.aud.got)
	}
}
