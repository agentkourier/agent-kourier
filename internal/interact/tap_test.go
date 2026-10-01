package interact_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// fakePauses is the intake: it records each Paused, with when, and answers as handle says (nil: success).
type fakePauses struct {
	mu     sync.Mutex
	got    []interact.Pause
	at     []time.Time
	handle func(ctx context.Context, n int, p interact.Pause) error // n counts the calls, from 1
}

func (f *fakePauses) Paused(ctx context.Context, p interact.Pause) error {
	f.mu.Lock()
	f.got, f.at = append(f.got, p), append(f.at, time.Now())
	n, h := len(f.got), f.handle
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	return h(ctx, n, p)
}

func (f *fakePauses) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// tapInner is the sink the tap wraps: the renderer. It keeps what it was given.
type tapInner struct {
	mu      sync.Mutex
	refs    []session.TurnRef
	events  []agent.Event
	notices []session.Notice
}

func (s *tapInner) Turn(_ context.Context, ref session.TurnRef) agent.Handler {
	s.mu.Lock()
	s.refs = append(s.refs, ref)
	s.mu.Unlock()
	return func(e agent.Event) { s.mu.Lock(); s.events = append(s.events, e); s.mu.Unlock() }
}

func (s *tapInner) Notice(_ context.Context, _ session.Key, n session.Notice) {
	s.mu.Lock()
	s.notices = append(s.notices, n)
	s.mu.Unlock()
}

func (s *tapInner) seen() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.events) }

// pausedTask is a task paused for input, whose pause is named by its status message and made at ts.
func pausedTask(id, msg string, ts time.Time) a2a.Task {
	return a2a.Task{ID: a2a.TaskID(id), ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &ts, Message: &a2a.Message{ID: msg}}}
}

var tapRef = session.TurnRef{Key: key, MessageID: "m1", UserID: "U1", Binding: "ns/b"}

type tapRig struct {
	tap    *interact.Tap
	inner  *tapInner
	pauses *fakePauses
	logs   *logBuf
}

// newTap builds a tap over fakes, whose Binding times a pause out after three minutes, on a clock that
// stands at d0 unless mod sets another, and closes it when the test ends.
func newTap(t *testing.T, mod func(*interact.TapConfig)) *tapRig {
	t.Helper()
	r := &tapRig{inner: &tapInner{}, pauses: &fakePauses{}, logs: &logBuf{}}
	cfg := interact.TapConfig{Sink: r.inner, Handler: r.pauses, Lookup: lookup{interact.Binding{Timeout: 3 * time.Minute}},
		Now: func() time.Time { return d0 }, Logger: slog.New(slog.NewTextHandler(r.logs, nil))}
	if mod != nil {
		mod(&cfg)
	}
	var err error
	if r.tap, err = interact.NewTap(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // bounded: a Close that waits for ever would hold the whole run to go test's timeout
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = r.tap.Close(ctx)
	})
	return r
}

// close ends the tap, which waits for what it started, and returns what Close did.
func (r *tapRig) close(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return r.tap.Close(ctx)
}

func TestAPausedFinalEventIsHandedOffOnceAndTheCallReturnsAtOnce(t *testing.T) {
	r := newTap(t, nil)
	gate := make(chan struct{})
	r.pauses.handle = func(context.Context, int, interact.Pause) error { <-gate; return nil } // the intake is slow
	t.Cleanup(func() { close(gate) })                                                         // before the tap is closed
	task := pausedTask("task-1", "msg-1", d0)
	h := r.tap.Turn(t.Context(), tapRef)
	returned := make(chan struct{})
	go func() { h(agent.FinalEvent{Task: &task}); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler waited for the intake: it runs on the agent's stream goroutine")
	}
	waitFor(t, "the hand-off", func() bool { return r.pauses.calls() == 1 })
	r.pauses.mu.Lock()
	got := r.pauses.got[0]
	r.pauses.mu.Unlock()
	if got.Key != key || got.Binding != "ns/b" || got.Task.ID != "task-1" || r.inner.seen() != 1 {
		t.Errorf("handed off %+v, the renderer saw %d events; want the thread's key, the Binding of the turn, the task, and the renderer given the event too", got, r.inner.seen())
	}
}

func TestOtherEventsAreNotHandedOff(t *testing.T) {
	working := pausedTask("task-1", "m", d0)
	working.Status.State = a2a.TaskStateWorking
	done := pausedTask("task-2", "m", d0)
	done.Status.State = a2a.TaskStateCompleted
	auth := pausedTask("task-3", "m", d0)
	auth.Status.State = a2a.TaskStateAuthRequired
	for name, ev := range map[string]agent.Event{
		"text":               agent.TextEvent{Text: "hello"},
		"a final of no task": agent.FinalEvent{},
		"a working task":     agent.FinalEvent{Task: &working},
		"a finished task":    agent.FinalEvent{Task: &done},
		"an auth pause":      agent.FinalEvent{Task: &auth},
	} {
		t.Run(name, func(t *testing.T) {
			r := newTap(t, nil)
			r.tap.Turn(t.Context(), tapRef)(ev)
			if err := r.close(t); err != nil { // waits for anything that was started
				t.Fatal(err)
			}
			if r.pauses.calls() != 0 || r.inner.seen() != 1 {
				t.Errorf("%d hand-offs, the renderer saw %d events; want none handed off and the event given to the renderer", r.pauses.calls(), r.inner.seen())
			}
		})
	}
}

func TestAFailingPauseIsTriedAgainAfterAGrowingWait(t *testing.T) {
	r := newTap(t, func(c *interact.TapConfig) { c.Backoff, c.MaxBackoff = 20*time.Millisecond, 60*time.Millisecond })
	r.pauses.handle = func(_ context.Context, n int, _ interact.Pause) error {
		if n < 5 {
			return errBoom
		}
		return nil
	}
	task := pausedTask("task-1", "msg-1", d0)
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "five tries", func() bool { return r.pauses.calls() == 5 })
	r.pauses.mu.Lock()
	at := append([]time.Time(nil), r.pauses.at...)
	r.pauses.mu.Unlock()
	for i, want := range []time.Duration{20, 40, 60, 60} { // doubling, to the cap
		if got := at[i+1].Sub(at[i]); got < want*time.Millisecond {
			t.Errorf("try %d came %v after the one before; want at least %v", i+2, got, want*time.Millisecond)
		}
	}
	if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "boom") {
		t.Errorf("log %q; want a warning that names the error", r.logs.String())
	}
}

// TestTheFirstWaitIsASecond: by default a failing pause is tried again after a second, and that doubles.
func TestTheFirstWaitIsASecond(t *testing.T) {
	r := newTap(t, nil)
	r.pauses.handle = func(context.Context, int, interact.Pause) error { return errBoom }
	task := pausedTask("task-1", "msg-1", d0)
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "the first try", func() bool { return r.pauses.calls() == 1 })
	time.Sleep(200 * time.Millisecond) // an immediate retry, or a wait of milliseconds, would have come by now
	if got := r.pauses.calls(); got != 1 {
		t.Errorf("%d tries after 200ms; want one: the first wait is a second", got)
	}
}

// TestAPauseIsGivenUpAtItsOwnDeadline: the deadline is the task's own time plus the Binding's timeout, as the
// interaction's is, so it does not move with the clock at the hand-off. After it the sweep owns the pause, or
// nothing does, and the tap says so, at Error.
func TestAPauseIsGivenUpAtItsOwnDeadline(t *testing.T) {
	var step atomic.Int64 // each failed try moves the clock on 30 seconds
	r := newTap(t, func(c *interact.TapConfig) {
		c.Backoff, c.MaxBackoff = time.Millisecond, time.Millisecond
		c.Now = func() time.Time { return d0.Add(2*time.Minute + time.Duration(step.Load())*30*time.Second) } // 2 minutes after the pause
	})
	r.pauses.handle = func(context.Context, int, interact.Pause) error { step.Add(1); return errBoom }
	task := pausedTask("task-1", "msg-1", d0) // due at d0 + 3 minutes
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "an Error log", func() bool { return strings.Contains(r.logs.String(), "level=ERROR") })
	if err := r.close(t); err != nil {
		t.Fatal(err)
	}
	if got := r.pauses.calls(); got != 2 { // at 2:30 it is not due; at 3:00 it is
		t.Errorf("%d tries; want 2: the second ends at the deadline", got)
	}
	if log := r.logs.String(); !strings.Contains(log, "task-1") || !strings.Contains(log, "boom") {
		t.Errorf("log %q; want the error to name the task and the cause", log)
	}
}

func TestAPauseTheIntakeCannotRenderIsNotTriedAgain(t *testing.T) {
	r := newTap(t, func(c *interact.TapConfig) { c.Backoff, c.MaxBackoff = time.Millisecond, time.Millisecond })
	r.pauses.handle = func(context.Context, int, interact.Pause) error { return interact.ErrDisallowed }
	task := pausedTask("task-1", "msg-1", d0)
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "an Error log", func() bool { return strings.Contains(r.logs.String(), "level=ERROR") })
	if err := r.close(t); err != nil || r.pauses.calls() != 1 {
		t.Errorf("Close = %v after %d tries; want one: no retry can change what the Binding allows", err, r.pauses.calls())
	}
}

// TestAPauseSeenAgainIsHandedOffAgain: nothing is remembered once a hand-off is over, because the intake is
// what knows what is done, and a pause that comes again after a restart is how an answered one is sent again
// (28h). A pause whose hand-off is still running is not started twice, and another pause of the same task is.
func TestAPauseSeenAgainIsHandedOffAgain(t *testing.T) {
	r := newTap(t, nil)
	task := pausedTask("task-1", "msg-1", d0)
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "the first hand-off", func() bool { return r.pauses.calls() == 1 })
	// A restart: a new tap, and the turn that finds the task again, marked recovered.
	again := newTap(t, nil)
	recovered := tapRef
	recovered.Recovered = true
	again.tap.Turn(t.Context(), recovered)(agent.FinalEvent{Task: &task})
	waitFor(t, "the hand-off after the restart", func() bool { return again.pauses.calls() == 1 })
	// And on the same tap, after the first is over.
	r.tap.Turn(t.Context(), recovered)(agent.FinalEvent{Task: &task})
	waitFor(t, "the repeat", func() bool { return r.pauses.calls() == 2 })
}

func TestAPauseInFlightIsNotHandedOffTwice(t *testing.T) {
	r := newTap(t, nil)
	gate := make(chan struct{})
	r.pauses.handle = func(context.Context, int, interact.Pause) error { <-gate; return nil }
	t.Cleanup(func() { close(gate) }) // before the tap is closed
	first, again, other := pausedTask("task-1", "msg-1", d0), pausedTask("task-1", "msg-1", d0), pausedTask("task-1", "msg-2", d0.Add(time.Minute))
	h := r.tap.Turn(t.Context(), tapRef)
	h(agent.FinalEvent{Task: &first})
	waitFor(t, "the first", func() bool { return r.pauses.calls() == 1 })
	h(agent.FinalEvent{Task: &again}) // the same pause, re-emitted while it is being taken up
	h(agent.FinalEvent{Task: &other}) // the task paused again, on a new question
	waitFor(t, "the other pause", func() bool { return r.pauses.calls() == 2 })
	time.Sleep(20 * time.Millisecond) // a third hand-off would have started by now
	if got := r.pauses.calls(); got != 2 {
		t.Errorf("%d hand-offs; want 2: one for each pause, and none for the repeat of one in flight", got)
	}
}

// TestCloseEndsTheHandOffsAndWaitsForThem: a hand-off in the intake is cut short through its context, one
// waiting out a backoff is stopped, and nothing more is started after.
func TestCloseEndsTheHandOffsAndWaitsForThem(t *testing.T) {
	before := goleak.IgnoreCurrent()
	r := newTap(t, func(c *interact.TapConfig) { c.Backoff, c.MaxBackoff = time.Hour, time.Hour })
	var inIntake atomic.Bool
	r.pauses.handle = func(ctx context.Context, n int, _ interact.Pause) error {
		if n == 1 { // the first fails, and waits an hour; the second is held in the intake
			return errBoom
		}
		inIntake.Store(true)
		<-ctx.Done()
		return ctx.Err()
	}
	h := r.tap.Turn(t.Context(), tapRef)
	one, two := pausedTask("task-1", "m", d0), pausedTask("task-2", "m", d0)
	h(agent.FinalEvent{Task: &one})
	waitFor(t, "the first, which fails", func() bool { return r.pauses.calls() == 1 })
	h(agent.FinalEvent{Task: &two})
	waitFor(t, "the second, in the intake", inIntake.Load)
	if err := r.close(t); err != nil {
		t.Fatalf("Close = %v; want nil once both are over", err)
	}
	h(agent.FinalEvent{Task: &one})
	if err := r.close(t); err != nil || r.pauses.calls() != 2 {
		t.Errorf("Close again = %v, %d calls; want nil, and nothing started after Close", err, r.pauses.calls())
	}
	goleak.VerifyNone(t, before)
}

// TestCloseIsBoundedByItsContext: an intake that ignores its context cannot hold the shutdown.
func TestCloseIsBoundedByItsContext(t *testing.T) {
	r := newTap(t, nil)
	stuck := make(chan struct{})
	r.pauses.handle = func(context.Context, int, interact.Pause) error { <-stuck; return nil }
	t.Cleanup(func() { close(stuck) })
	task := pausedTask("task-1", "m", d0)
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "the hand-off", func() bool { return r.pauses.calls() == 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := r.tap.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v; want the context's error, not a wait for ever", err)
	}
}

func TestABindingThatCannotBeReadStillGetsTheHandOffAndAWarning(t *testing.T) {
	r := newTap(t, func(c *interact.TapConfig) { c.Lookup = lookupErr{errBoom} })
	task := pausedTask("task-1", "msg-1", d0)
	r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task})
	waitFor(t, "the hand-off", func() bool { return r.pauses.calls() == 1 })
	if !strings.Contains(r.logs.String(), "level=WARN") {
		t.Errorf("log %q; want a warning that the Binding could not be read", r.logs.String())
	}
}

func TestNoticesGoToTheSinkItWraps(t *testing.T) {
	r := newTap(t, nil)
	r.tap.Notice(t.Context(), key, session.Notice{ID: "n1", Kind: session.NoticeExpired})
	if len(r.inner.notices) != 1 || r.inner.notices[0].ID != "n1" {
		t.Errorf("notices %+v; want the one given to the tap, passed on", r.inner.notices)
	}
}

type lookupErr struct{ err error }

func (l lookupErr) Binding(string) (interact.Binding, error) { return interact.Binding{}, l.err }

// gatedLookup holds every Binding lookup until its gate is closed.
type gatedLookup struct{ gate chan struct{} }

func (l gatedLookup) Binding(string) (interact.Binding, error) {
	<-l.gate
	return interact.Binding{Timeout: 3 * time.Minute}, nil
}

// TestTheBindingIsReadOffTheStreamGoroutine: the lookup, which may log, is the hand-off's to make and not the
// handler's, which runs on the agent's stream goroutine and must return at once.
func TestTheBindingIsReadOffTheStreamGoroutine(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	r := newTap(t, func(c *interact.TapConfig) { c.Lookup = gatedLookup{gate} })
	t.Cleanup(release) // before the tap is closed
	task := pausedTask("task-1", "msg-1", d0)
	h := r.tap.Turn(t.Context(), tapRef)
	returned := make(chan struct{})
	go func() { h(agent.FinalEvent{Task: &task}); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler waited for the Binding lookup")
	}
	release()
	waitFor(t, "the hand-off", func() bool { return r.pauses.calls() == 1 })
}
