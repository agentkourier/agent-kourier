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
	"github.com/agentkourier/agent-kourier/internal/store"
)

// fakeProc is a PressProcessor that does what handle says, and keeps what it was asked.
type fakeProc struct {
	mu     sync.Mutex
	handle func(ctx context.Context, p store.Press) (interact.Result, error)
	calls  []int64 // the Seq of each Process, in the order they began
}

func (f *fakeProc) Process(ctx context.Context, p store.Press) (interact.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p.Seq)
	f.mu.Unlock()
	return f.handle(ctx, p)
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

// pr is a press of an interaction, made at d0.
func pr(seq int64, id string) store.Press {
	return store.Press{Seq: seq, InteractionID: id, User: "UALICE1", Channel: "C1", MessageTS: "100.1", PressedAt: d0}
}

// startWorker runs a Worker until the test ends or stop is called. It returns what Run returned.
func startWorker(t *testing.T, p interact.PressProcessor, mod func(*interact.WorkerConfig)) (w *interact.Worker, logs *logBuf, stop func() error) {
	t.Helper()
	logs = &logBuf{}
	cfg := interact.WorkerConfig{Processor: p, Logger: slog.New(slog.NewTextHandler(logs, nil))}
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

// TestShuttingDownLeavesAPressRecorded: a Process cut off by the context is not a failure of the press, and
// nothing is left running.
func TestShuttingDownLeavesAPressRecorded(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := &fakeProc{handle: func(ctx context.Context, _ store.Press) (interact.Result, error) {
		<-ctx.Done()
		return interact.Retry, ctx.Err()
	}}
	w, logs, stop := startWorker(t, f, nil)
	w.Submit(pr(1, "ix-1"))
	w.Submit(pr(2, "ix-2"))
	waitFor(t, "both presses", func() bool { return len(f.processed()) == 2 })
	if err := stop(); err == nil {
		t.Error("Run returned nil; want the context's error")
	}
	if strings.Contains(logs.String(), "could not be processed") {
		t.Errorf("log %q; want a press cut off by shutdown not reported as a failure", logs.String())
	}
	w.Submit(pr(3, "ix-3")) // after Run: ignored, and recorded for the next start
	goleak.VerifyNone(t, before)
}

// TestAPressThatCannotBeProcessedYetIsLeftRecordedAndTheLaneGoesOn: nothing here tries it again.
func TestAPressThatCannotBeProcessedYetIsLeftRecordedAndTheLaneGoesOn(t *testing.T) {
	f := &fakeProc{handle: func(_ context.Context, p store.Press) (interact.Result, error) {
		if p.Seq == 1 {
			return interact.Retry, errBoom
		}
		return interact.Done, nil
	}}
	w, logs, _ := startWorker(t, f, nil)
	w.Submit(pr(1, "ix-1"))
	w.Submit(pr(2, "ix-1"))
	waitFor(t, "the later press", func() bool { return f.processed()[2] == 1 })
	if f.processed()[1] != 1 || !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "boom") {
		t.Errorf("tried %v, log %q; want press 1 tried once and a warning that names the error", f.processed(), logs.String())
	}
}
