package bindingcheck

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// These tests run in a synctest bubble: time is fake and moves only when every goroutine is blocked, so the
// production clock path is the one under test. synctest.Wait returns when the Prober is idle.

const (
	tick     = 5 * time.Second
	timeout  = 10 * time.Second
	interval = time.Minute
)

// fakeAgents and fakeChannels are checkers that count their calls and answer through fn.
type fakeAgents struct {
	mu    sync.Mutex
	calls map[string]int
	fn    func(ctx context.Context, binding string) (Outcome, error)
}

func (f *fakeAgents) CheckAgent(ctx context.Context, b string) (Outcome, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[b]++
	f.mu.Unlock()
	if f.fn == nil {
		return ok(), nil
	}
	return f.fn(ctx, b)
}

func (f *fakeAgents) count(b string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[b]
}

func (f *fakeAgents) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

type fakeChannels struct {
	mu    sync.Mutex
	calls map[string]int
	fn    func(ctx context.Context, channel string) (Outcome, error)
}

func (f *fakeChannels) CheckChannel(ctx context.Context, _, ch string) (Outcome, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[ch]++
	f.mu.Unlock()
	if f.fn == nil {
		return ok(), nil
	}
	return f.fn(ctx, ch)
}

func (f *fakeChannels) count(ch string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ch]
}

func ok() Outcome { return Outcome{Status: True, Reason: ReasonOK} }

// gauge counts how many calls are inside it at once, and the most that ever were.
type gauge struct{ cur, most atomic.Int32 }

func (g *gauge) enter() (leave func()) {
	n := g.cur.Add(1)
	for {
		m := g.most.Load()
		if n <= m || g.most.CompareAndSwap(m, n) {
			break
		}
	}
	return func() { g.cur.Add(-1) }
}

// targets is a mutable Targets and Revision source.
type targets struct {
	mu  sync.Mutex
	t   []Target
	rev string
}

func (g *targets) Targets() []Target {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.t)
}

func (g *targets) Revision(context.Context) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rev
}

func (g *targets) set(rev string, t ...Target) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rev, g.t = rev, t
}

// harness runs a Prober for the length of a test body.
type harness struct {
	t      *testing.T
	p      *Prober
	cancel context.CancelFunc
	done   chan struct{}
}

// run starts a Prober with o inside a bubble, waits for it to go idle, and calls body. Interval defaults to a
// minute so that tests do not wait five.
func run(t *testing.T, o Options, body func(h *harness)) {
	t.Helper()
	runIn(t, func() Options { return o }, body)
}

// runIn is run for a test whose Options use channels: a channel made outside the bubble does not count as durably
// blocking, so setup makes them inside it.
func runIn(t *testing.T, setup func() Options, body func(h *harness)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		o := setup()
		if o.Interval == 0 {
			o.Interval = interval
		}
		h := &harness{t: t, p: NewProber(o), done: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go func() { defer close(h.done); h.p.Run(ctx) }()
		defer h.stop()
		synctest.Wait()
		body(h)
	})
}

// stop ends Run and waits for it to return. The bubble then fails the test if any goroutine of the Prober is left.
func (h *harness) stop() {
	h.cancel()
	<-h.done
	synctest.Wait()
}

// advance moves the fake clock by d and waits for the Prober to go idle.
func (h *harness) advance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

// nextPass advances one Tick at a time until count() grows, which is when a pass has begun, and returns once the
// Prober is idle again.
func (h *harness) nextPass(count func() int) {
	h.t.Helper()
	before := count()
	for range 30 {
		h.advance(tick)
		if count() > before {
			return
		}
	}
	h.t.Fatalf("no pass began within %d ticks", 30)
}

func (h *harness) conds(binding string) map[ConditionType]Condition {
	h.t.Helper()
	out := map[ConditionType]Condition{}
	for _, c := range h.p.State().Snapshot()[binding] {
		out[c.Type] = c
	}
	return out
}

func wantCond(t *testing.T, got map[ConditionType]Condition, typ ConditionType, s Status, r Reason) {
	t.Helper()
	if c, found := got[typ]; !found || c.Status != s || c.Reason != r {
		t.Errorf("%s = %s/%s (present %v), want %s/%s", typ, c.Status, c.Reason, found, s, r)
	}
}

func TestJitteredStaysWithinTenPercent(t *testing.T) {
	const iv = 5 * time.Minute
	lo, hi := iv*9/10, iv*11/10
	tests := []struct {
		r    float64
		want time.Duration // 0: only the bounds are checked
	}{{0, lo}, {0.25, iv * 95 / 100}, {0.5, iv}, {math.Nextafter(1, 0), 0}}
	for _, tt := range tests {
		got := jittered(iv, tt.r)
		if tt.want != 0 && got != tt.want {
			t.Errorf("jittered(r=%v) = %v, want %v", tt.r, got, tt.want)
		}
		if got < lo || got > hi {
			t.Errorf("jittered(r=%v) = %v, outside [%v, %v]", tt.r, got, lo, hi)
		}
	}
}

func TestNewProberDefaults(t *testing.T) {
	o := NewProber(Options{}).o
	if o.Interval != 5*time.Minute || o.Tick != 5*time.Second || o.Timeout != 10*time.Second || o.Concurrency != 4 {
		t.Errorf("defaults = interval %v tick %v timeout %v concurrency %d", o.Interval, o.Tick, o.Timeout, o.Concurrency)
	}
}

func TestPassesKeepToTheJitteredInterval(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var mu sync.Mutex
	var starts []time.Time
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		mu.Lock()
		defer mu.Unlock()
		starts = append(starts, time.Now())
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		for range 40 {
			h.nextPass(agents.total)
		}
		mu.Lock()
		defer mu.Unlock()
		distinct := map[time.Duration]bool{}
		for i := 1; i < len(starts); i++ {
			gap := starts[i].Sub(starts[i-1])
			// 54s to 66s, seen through a 5s tick.
			if gap < 54*time.Second || gap > 70*time.Second {
				t.Errorf("gap %d = %v, outside the 10%% jitter of a minute", i, gap)
			}
			distinct[gap] = true
		}
		if len(distinct) < 2 {
			t.Errorf("all %d gaps were alike (%v): no jitter", len(starts)-1, distinct)
		}
	})
}

func TestProbesAtStartAndOnTheInterval(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents, chans := &fakeAgents{}, &fakeChannels{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans}, func(h *harness) {
		if agents.count("b") != 1 || chans.count("C1") != 1 {
			t.Fatalf("probes at start = %d agent, %d channel; want 1 each", agents.count("b"), chans.count("C1"))
		}
		c := h.conds("b")
		wantCond(t, c, BackendReachable, True, ReasonOK)
		wantCond(t, c, ChannelJoined, True, ReasonOK)
		wantCond(t, c, Ready, True, ReasonOK)

		h.advance(50 * time.Second) // under 90% of the interval
		if agents.count("b") != 1 {
			t.Fatalf("probed again after 50s of a minute")
		}
		h.advance(20 * time.Second) // 70s: over 110%, and no second pass can fit between
		if agents.count("b") != 2 || chans.count("C1") != 2 {
			t.Errorf("probes at the interval = %d agent, %d channel; want 2 each", agents.count("b"), chans.count("C1"))
		}
		if got := h.conds("b")[BackendReachable].LastProbeTime; got.Before(time.Now().Add(-interval)) || got.IsZero() {
			t.Errorf("LastProbeTime = %v", got)
		}
	})
}

func TestUnchangedRevisionDoesNotProbeEarlyAndChangedOneDoesAtTheNextTick(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		h.advance(3 * tick)
		if got := agents.count("b"); got != 1 {
			t.Fatalf("probes with an unchanged revision = %d, want 1", got)
		}

		g.set("r2", Target{Binding: "b", Channel: "C1"})
		synctest.Wait()
		if got := agents.count("b"); got != 1 {
			t.Fatalf("the probe ran before the tick: %d", got)
		}
		h.advance(tick)
		if got := agents.count("b"); got != 2 {
			t.Errorf("probes after a revision change = %d, want 2", got)
		}
		h.advance(tick)
		if got := agents.count("b"); got != 2 {
			t.Errorf("probes after the revision settled = %d, want 2", got)
		}
	})
}

// A change of Revision inside the gap since the last pass began waits for the gap to end and is then probed once, for the last
// revision: many changes in the gap are one pass, and none is lost. The first pass, and a pass the Interval calls for, are not held
// back by the gap.
func TestRevisionChangesInsideTheMinimumGapGiveOnePassWhenTheGapEndsAndNeverLoseTheLast(t *testing.T) {
	const gap = 30 * time.Second
	g := &targets{}
	g.set("r0", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, MinRevisionGap: gap, Interval: time.Hour}, func(h *harness) {
		if got := agents.count("b"); got != 1 {
			t.Fatalf("the first pass made %d probes, want 1: the gap does not hold it back", got)
		}
		// Many changes in the gap, a Tick apart.
		for i := 1; i <= 4; i++ {
			g.set(fmt.Sprintf("r%d", i), Target{Binding: "b", Channel: "C1"})
			h.advance(tick)
		}
		if got := agents.count("b"); got != 1 {
			t.Fatalf("%d probes 20s after the first pass with revisions changing; want 1: the gap is 30s", got)
		}
		h.advance(gap) // well past the gap since the first pass began
		if got := agents.count("b"); got != 2 {
			t.Fatalf("%d probes after the gap; want 2: one pass for all the changes", got)
		}
		// A change just inside the next gap waits, and a later one in it replaces it: the pass at the end of the gap is for the last.
		g.set("r5", Target{Binding: "b", Channel: "C1"})
		h.advance(tick)
		if got := agents.count("b"); got != 2 {
			t.Fatalf("%d probes inside the gap; want 2", got)
		}
		g.set("r6", Target{Binding: "b", Channel: "C1"})
		h.advance(gap)
		if got := agents.count("b"); got != 3 {
			t.Errorf("%d probes after the second gap; want 3: the last change must still be probed", got)
		}
		h.advance(gap)
		if got := agents.count("b"); got != 3 {
			t.Errorf("%d probes with nothing changed; want 3", got)
		}
	})
}

// A pass the Interval calls for is not held back by the gap, however long the gap.
func TestTheMinimumGapDoesNotHoldBackTheInterval(t *testing.T) {
	g := &targets{}
	g.set("r0", Target{Binding: "a", Channel: "C1"})
	agents := &fakeAgents{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, MinRevisionGap: 10 * time.Minute, Interval: time.Minute}, func(h *harness) {
		h.advance(2 * time.Minute) // past the Interval, inside the gap
		if got := agents.count("a"); got < 2 {
			t.Errorf("%d probes after two intervals; want at least 2: the gap is for changes", got)
		}
	})
}

// A Binding that appears in Targets is a change like any other: inside the gap it waits, its conditions Pending, and is probed when the
// gap ends, not lost (the appearance is reported once, and is kept until a pass runs). Pruning is not held up by the gap, and a Binding
// that leaves Targets does not call for a pass.
func TestANewBindingInsideTheMinimumGapWaitsPendingAndIsProbedWhenItEnds(t *testing.T) {
	const gap = 30 * time.Second
	g := &targets{}
	g.set("r0", Target{Binding: "a", Channel: "C1"}, Target{Binding: "gone", Channel: "C9"})
	agents := &fakeAgents{}
	var pruned []string
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, MinRevisionGap: gap, Interval: time.Hour,
		OnPrune: func(names []string) { pruned = append(pruned, names...) }}, func(h *harness) {
		if got := agents.count("a"); got != 1 {
			t.Fatalf("the first pass made %d probes of a; want 1", got)
		}
		h.advance(tick)
		g.set("r0", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"}) // b appears, the revision unchanged; gone leaves
		h.advance(tick)
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonPending)
		if got := agents.count("b"); got != 0 {
			t.Fatalf("the new Binding was probed %d times inside the gap; want 0", got)
		}
		if !slices.Equal(pruned, []string{"gone"}) {
			t.Errorf("pruned %v inside the gap; want [gone]: pruning is not held up", pruned)
		}
		if got := agents.count("a"); got != 1 {
			t.Errorf("%d probes of a inside the gap; want 1: a Binding that left needs no pass", got)
		}
		h.advance(gap) // the gap since the first pass is over; the appearance, reported once, is still kept
		if got := agents.count("b"); got != 1 {
			t.Errorf("the new Binding was probed %d times when the gap ended; want 1", got)
		}
		wantCond(t, h.conds("b"), BackendReachable, True, ReasonOK)
		h.advance(gap)
		if got := agents.count("b"); got != 1 {
			t.Errorf("the new Binding was probed %d times with nothing changed; want 1", got)
		}
	})
}

func TestARevisionThatBlocksDoesNotStopTheProbes(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{}
	var blocked atomic.Int32
	rev := func(ctx context.Context) string {
		blocked.Add(1)
		<-ctx.Done() // a Secret read that never answers
		return "from a read that failed"
	}
	run(t, Options{Targets: g.Targets, Revision: rev, Agents: agents}, func(h *harness) {
		if agents.total() != 0 {
			t.Fatal("probed before the revision call ended")
		}
		h.advance(timeout)
		if agents.count("b") != 1 {
			t.Fatalf("probes after the revision timed out = %d, want 1", agents.count("b"))
		}
		// A call that outlasts the timeout is no change, so only the interval probes again.
		h.nextPass(agents.total)
		if agents.count("b") != 2 || blocked.Load() < 3 {
			t.Errorf("probes = %d, revision calls = %d; want the loop to keep going", agents.count("b"), blocked.Load())
		}
	})
}

func TestARevisionThatBlocksThenAnswersIsTakenOnTheNextTick(t *testing.T) {
	var state atomic.Int32 // 0 answers r1, 1 blocks, 2 answers r2
	rev := func(ctx context.Context) string {
		switch state.Load() {
		case 0:
			return "r1"
		case 1:
			<-ctx.Done()
			return "junk from a failed read"
		}
		return "r2"
	}
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{}
	run(t, Options{Targets: g.Targets, Revision: rev, Agents: agents, Interval: time.Hour}, func(h *harness) {
		if agents.total() != 1 {
			t.Fatalf("probes at start = %d, want 1", agents.total())
		}
		state.Store(1)
		h.advance(time.Minute)
		if agents.total() != 1 {
			t.Errorf("a Revision call that timed out ran a pass (%d probes): its answer was believed or read as empty", agents.total())
		}
		state.Store(2)
		h.advance(timeout + tick) // the blocked call ends within the timeout, then at most one tick
		if agents.total() != 2 {
			t.Errorf("probes after the Revision recovered = %d, want 2", agents.total())
		}
	})
}

func TestAReloadBetweenTheTwoReadsIsProbedAtOnce(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var reload atomic.Bool
	rev := func(ctx context.Context) string {
		if reload.CompareAndSwap(true, false) {
			g.set("r2", Target{Binding: "b", Channel: "C2"}) // the reload lands while the loop reads
		}
		return g.Revision(ctx)
	}
	chans := &fakeChannels{}
	run(t, Options{Targets: g.Targets, Revision: rev, Agents: &fakeAgents{}, Channels: chans, Interval: time.Hour}, func(h *harness) {
		reload.Store(true)
		h.advance(tick)
		if chans.count("C2") != 1 {
			t.Errorf("the channel the reload brought was probed %d times at the next tick, want 1", chans.count("C2"))
		}
	})
}

func TestRunWaitsForACheckerThatHonorsItsContext(t *testing.T) {
	var entered, finished atomic.Bool
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		entered.Store(true)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // closing a connection, say
		finished.Store(true)
		return Outcome{}, ctx.Err()
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Timeout: time.Hour}, func(h *harness) {
		if !entered.Load() {
			t.Fatal("the checker did not start")
		}
		h.cancel()
		<-h.done // not h.stop: its synctest.Wait would let the checker finish before the assertion
		if !finished.Load() {
			t.Error("Run returned while a checker that honors its context was still running")
		}
	})
}

func TestRunDoesNotWaitLongerThanTheGraceForACheckerThatIgnoresItsContext(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var gate chan struct{}
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) { <-gate; return ok(), nil }}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Timeout: time.Hour}
	}, func(h *harness) {
		began := time.Now()
		h.cancel()
		<-h.done
		if waited := time.Since(began); waited != shutdownGrace {
			t.Errorf("Run waited %v for a checker that ignores its context, want the grace, %v", waited, shutdownGrace)
		}
		close(gate)
	})
}

func TestRunWaitsForACheckerThatOutlastedItsProbesTimeout(t *testing.T) {
	var entered, finished atomic.Bool
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		entered.Store(true)
		<-ctx.Done()                       // the probe's timeout
		time.Sleep(500 * time.Millisecond) // unwinding takes a while, but inside the grace
		finished.Store(true)
		return Outcome{}, ctx.Err()
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		if !entered.Load() {
			t.Fatal("the checker did not start")
		}
		h.advance(timeout + 100*time.Millisecond) // the probe has timed out and returned; its checker is unwinding
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeTimeout)
		if finished.Load() {
			t.Fatal("the checker finished before the shutdown; the test proves nothing")
		}
		h.cancel()
		<-h.done
		if !finished.Load() {
			t.Error("Run returned while a checker that honors its context was still unwinding after its probe timed out")
		}
	})
}

func TestRunWaitsOnceForCheckersThatIgnoreTheirContextEvenAfterTheirProbesTimedOut(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
	var gate chan struct{}
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) { <-gate; return ok(), nil }}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}
	}, func(h *harness) {
		h.advance(timeout)
		wantCond(t, h.conds("a"), BackendReachable, Unknown, ReasonProbeTimeout)
		began := time.Now()
		h.cancel()
		<-h.done
		if waited := time.Since(began); waited != shutdownGrace {
			t.Errorf("Run waited %v for two checkers that ignore their contexts, want one grace, %v", waited, shutdownGrace)
		}
		close(gate)
	})
}

func TestAGoexitInACheckerFreesItsTargetForTheNextPass(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var exits atomic.Int32
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		if exits.Add(1) == 1 {
			runtime.Goexit() // what a test fake's t.Fatal does
		}
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeFailed)
		h.nextPass(agents.total)
		wantCond(t, h.conds("b"), BackendReachable, True, ReasonOK)
	})
}

func TestARevisionThatIgnoresItsContextIsNotCalledAgainAndDoesNotStopTheProbes(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	var gate chan struct{}
	var calls atomic.Int32
	agents := &fakeAgents{}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Agents: agents, Revision: func(context.Context) string {
			calls.Add(1)
			<-gate
			return "late"
		}}
	}, func(h *harness) {
		h.advance(timeout)
		if agents.count("a") != 1 {
			t.Fatalf("probes after the Revision call timed out = %d, want 1", agents.count("a"))
		}
		g.set("r2", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
		h.advance(tick)
		if agents.count("b") != 1 {
			t.Errorf("a Binding added while Revision is stuck was probed %d times at the next tick, want 1", agents.count("b"))
		}
		h.advance(time.Minute)
		if calls.Load() != 1 {
			t.Errorf("Revision was called %d times, want 1: a stuck call must not be followed by another", calls.Load())
		}
		began := time.Now()
		h.cancel()
		<-h.done
		if waited := time.Since(began); waited != shutdownGrace {
			t.Errorf("Run waited %v for a Revision call that ignores its context, want the grace, %v", waited, shutdownGrace)
		}
		close(gate)
	})
}

func TestARevisionThatPanicsCountsAsNoChangeAndIsReported(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	agents := &fakeAgents{}
	var mu sync.Mutex
	var perrs []ProbeError
	// It answers once, so that a panic's empty answer, if believed, would differ from the last revision.
	var calls atomic.Int32
	revision := func(context.Context) string {
		if calls.Add(1) == 1 {
			return "r1"
		}
		panic("revision boom")
	}
	run(t, Options{Targets: g.Targets, Agents: agents, Revision: revision, OnProbeError: func(pe ProbeError) {
		mu.Lock()
		defer mu.Unlock()
		perrs = append(perrs, pe)
	}}, func(h *harness) {
		h.advance(3 * tick)
		if got := agents.total(); got != 1 {
			t.Errorf("probes after Revision panicked = %d, want the first pass's 1: a panic is no change", got)
		}
		h.nextPass(agents.total) // the loop survived and the interval still probes
		mu.Lock()
		defer mu.Unlock()
		var panicked *PanicError
		if len(perrs) == 0 || !errors.As(perrs[0].Err, &panicked) || panicked.Value != "revision boom" || perrs[0].Condition != "" || !reflect.DeepEqual(perrs[0].Target, Target{}) {
			t.Errorf("OnProbeError calls = %+v, want a PanicError for Revision with no condition or targets", perrs)
		}
	})
}

func TestATargetsThatPanicsKeepsTheLastTargetsAndPrunesNothing(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
	var broken atomic.Bool
	agents := &fakeAgents{}
	var mu sync.Mutex
	var pruned [][]string
	var perrs []ProbeError
	run(t, Options{Agents: agents, Revision: g.Revision, Targets: func() []Target {
		if broken.Load() {
			panic("targets boom")
		}
		return g.Targets()
	}, OnPrune: func(names []string) {
		mu.Lock()
		defer mu.Unlock()
		pruned = append(pruned, names)
	}, OnProbeError: func(pe ProbeError) {
		mu.Lock()
		defer mu.Unlock()
		perrs = append(perrs, pe)
	}}, func(h *harness) {
		broken.Store(true)
		h.advance(2 * tick)
		if n := len(h.p.State().Snapshot()); n != 2 {
			t.Fatalf("Bindings after Targets panicked = %d, want 2: a panic must not read as no targets", n)
		}
		before := agents.count("b")
		h.nextPass(agents.total) // the last good targets are still probed
		if agents.count("b") != before+1 {
			t.Errorf("b was probed %d more times, want 1", agents.count("b")-before)
		}
		mu.Lock()
		if len(pruned) != 0 {
			t.Errorf("OnPrune = %v while Targets panicked", pruned)
		}
		var panicked *PanicError
		if len(perrs) == 0 || !errors.As(perrs[0].Err, &panicked) || panicked.Value != "targets boom" {
			t.Errorf("OnProbeError calls = %+v, want a PanicError for Targets", perrs)
		}
		mu.Unlock()

		broken.Store(false) // recovered: an honest change prunes again
		g.set("r2", Target{Binding: "a", Channel: "C1"})
		h.advance(tick)
		mu.Lock()
		defer mu.Unlock()
		if len(pruned) != 1 || !slices.Equal(pruned[0], []string{"b"}) {
			t.Errorf("OnPrune after Targets recovered = %v, want [[b]]", pruned)
		}
	})
}

func TestAHookThatPanicsCannotStopTheProberAndOnProbeErrorDoesNotRecurse(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "b" {
			return Outcome{}, errors.New("down")
		}
		return ok(), nil
	}}
	var mu sync.Mutex
	var perrs []ProbeError
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents,
		OnTransition: func(Transition) { panic("hook boom") },
		OnPrune:      func([]string) { panic("hook boom") },
		OnProbeError: func(pe ProbeError) {
			mu.Lock()
			perrs = append(perrs, pe)
			mu.Unlock()
			panic("hook boom")
		},
	}, func(h *harness) {
		wantCond(t, h.conds("a"), BackendReachable, True, ReasonOK)
		wantCond(t, h.conds("a"), Ready, True, ReasonOK)
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeFailed)
		g.set("r2", Target{Binding: "a", Channel: "C1"})
		h.advance(tick)
		if snap := h.p.State().Snapshot(); len(snap) != 1 {
			t.Errorf("after the prune hook panicked: %+v", snap)
		}
		h.nextPass(agents.total)
		mu.Lock()
		defer mu.Unlock()
		var fromTransition bool
		for _, pe := range perrs {
			var panicked *PanicError
			if errors.As(pe.Err, &panicked) && panicked.Value == "hook boom" && pe.Condition == BackendReachable && reflect.DeepEqual(pe.Target, Target{Binding: "a", Channel: "C1"}) {
				fromTransition = true
			}
		}
		if !fromTransition {
			t.Errorf("OnProbeError calls = %+v, want one reporting the panic of OnTransition", perrs)
		}
	})
}

// errorLog collects what OnProbeError is given.
type errorLog struct {
	mu   sync.Mutex
	errs []ProbeError
}

func (l *errorLog) add(pe ProbeError) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, pe)
}

func (l *errorLog) all() []ProbeError {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.errs)
}

func TestARevisionThatEndsItsGoroutineIsReportedNotBelievedAndCalledAgain(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	var calls atomic.Int32
	rev := func(context.Context) string {
		if calls.Add(1) == 2 {
			runtime.Goexit() // what a test fake's t.Fatal does
		}
		return "r1"
	}
	agents := &fakeAgents{}
	var log errorLog
	run(t, Options{Targets: g.Targets, Revision: rev, Agents: agents, OnProbeError: log.add}, func(h *harness) {
		h.advance(3 * tick) // the Goexit call, then two calls that answer r1
		if calls.Load() < 4 {
			t.Errorf("Revision calls = %d, want the loop to call it again after the Goexit", calls.Load())
		}
		if agents.total() != 1 {
			t.Errorf("probes = %d, want 1: the Goexit's empty answer was believed as a new revision", agents.total())
		}
		if errs := log.all(); len(errs) != 1 || errs[0].Err == nil || errs[0].Condition != "" || !reflect.DeepEqual(errs[0].Target, Target{}) {
			t.Errorf("OnProbeError calls = %+v, want one for the Revision call that ended", errs)
		}
	})
}

func TestARevisionInFlightAtShutdownIsNotAFailure(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	var log errorLog
	run(t, Options{Targets: g.Targets, Agents: &fakeAgents{}, Revision: func(ctx context.Context) string {
		<-ctx.Done()
		return "junk"
	}, OnProbeError: log.add}, func(h *harness) {
		h.stop()
		if errs := log.all(); len(errs) != 0 {
			t.Errorf("OnProbeError calls = %+v for a Revision call cut off by the shutdown", errs)
		}
	})
}

func TestARevisionThatTimesOutIsReportedOncePerCall(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	var log errorLog
	run(t, Options{Targets: g.Targets, Agents: &fakeAgents{}, Revision: func(ctx context.Context) string {
		<-ctx.Done()
		return "junk"
	}, OnProbeError: log.add}, func(h *harness) {
		h.advance(timeout)
		errs := log.all()
		if len(errs) != 1 || errs[0].Err == nil || errs[0].Condition != "" || !reflect.DeepEqual(errs[0].Target, Target{}) {
			t.Errorf("OnProbeError calls = %+v, want one for the Revision call that timed out", errs)
		}
	})
}

func TestATargetsThatPanicsDoesNotConsumeANewRevision(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var broken atomic.Bool
	agents, chans := &fakeAgents{}, &fakeChannels{}
	run(t, Options{Agents: agents, Channels: chans, Revision: g.Revision, Targets: func() []Target {
		if broken.Load() {
			panic("targets boom")
		}
		return g.Targets()
	}}, func(h *harness) {
		broken.Store(true)
		g.set("r2", Target{Binding: "b", Channel: "C2"}) // the Binding moves while Targets is down
		h.advance(3 * tick)
		if agents.total() != 1 {
			t.Fatalf("probes while Targets panicked = %d, want 1: no pass runs for a revision that cannot be probed", agents.total())
		}
		broken.Store(false)
		h.advance(tick)
		if chans.count("C2") != 1 {
			t.Errorf("the new channel was probed %d times at the first tick after Targets recovered, want 1", chans.count("C2"))
		}
	})
}

func TestATargetsThatPanicsOnTheFirstTickProbesAtTheNextTickItWorks(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	var broken atomic.Bool
	broken.Store(true)
	agents := &fakeAgents{}
	run(t, Options{Agents: agents, Revision: g.Revision, Targets: func() []Target {
		if broken.Load() {
			panic("targets boom")
		}
		return g.Targets()
	}}, func(h *harness) {
		if agents.total() != 0 || len(h.p.State().Snapshot()) != 0 {
			t.Fatalf("probes = %d, Bindings = %d, want an empty pass", agents.total(), len(h.p.State().Snapshot()))
		}
		broken.Store(false)
		h.advance(tick)
		if agents.count("a") != 1 {
			t.Errorf("probes at the first working tick = %d, want 1: the Binding is new", agents.count("a"))
		}
		wantCond(t, h.conds("a"), BackendReachable, True, ReasonOK)
	})
}

func TestAnOnPrunePanicIsReported(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
	var log errorLog
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, OnPrune: func([]string) { panic("prune boom") }, OnProbeError: log.add}, func(h *harness) {
		g.set("r2", Target{Binding: "a", Channel: "C1"})
		h.advance(tick)
		errs := log.all()
		var panicked *PanicError
		if len(errs) != 1 || !errors.As(errs[0].Err, &panicked) || panicked.Value != "prune boom" || errs[0].Condition != "" || !reflect.DeepEqual(errs[0].Target, Target{}) {
			t.Errorf("OnProbeError calls = %+v, want one PanicError for OnPrune", errs)
		}
	})
}

func TestAnOnTransitionPanicDoesNotSkipTheTransitionsAndBindingsThatFollow(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C1"}, Target{Binding: "c", Channel: "C1"})
	var log errorLog
	var transitions atomic.Int32
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Channels: &fakeChannels{}, OnProbeError: log.add, OnTransition: func(Transition) {
		transitions.Add(1)
		panic("transition boom")
	}}, func(h *harness) {
		for _, b := range []string{"a", "b", "c"} {
			wantCond(t, h.conds(b), ChannelJoined, True, ReasonOK)
			wantCond(t, h.conds(b), Ready, True, ReasonOK)
		}
		// Each Binding changes ChannelJoined and Ready, and every change is announced although the first panicked.
		if transitions.Load() != 6 {
			t.Errorf("OnTransition calls = %d, want 6", transitions.Load())
		}
		reported := map[string]bool{}
		for _, pe := range log.all() {
			if !reflect.DeepEqual(pe.Target, Target{}) {
				reported[pe.Target.Binding] = true
			}
		}
		if len(reported) != 3 {
			t.Errorf("OnTransition panics reported for %v, want a, b and c", reported)
		}
	})
}

func TestPassesDoNotOverlap(t *testing.T) {
	g := &targets{}
	// "quick" answers at once, so a second pass beside the first would probe it again: "b" alone could not show one, as
	// its own probe is still running and is not started twice.
	g.set("r1", Target{Binding: "b", Channel: "C1"}, Target{Binding: "quick", Channel: "C2"})
	var in gauge
	var gate chan struct{}
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "quick" {
			return ok(), nil
		}
		defer in.enter()()
		<-gate
		return ok(), nil
	}}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Concurrency: 4, Timeout: time.Hour}
	}, func(h *harness) {
		if in.cur.Load() != 1 {
			t.Fatalf("in flight = %d, want the first probe", in.cur.Load())
		}
		// The revision changes and the interval runs out while the pass is still going.
		g.set("r2", Target{Binding: "b", Channel: "C1"}, Target{Binding: "quick", Channel: "C2"})
		h.advance(2 * interval)
		if got := agents.count("quick"); got != 1 {
			t.Fatalf("a second pass began during the first: %d probes of the other Binding", got)
		}
		gate <- struct{}{}
		synctest.Wait()
		h.advance(tick) // the next tick sees the new revision
		if got := agents.count("b"); got != 2 {
			t.Fatalf("probes after the first pass ended = %d, want a second pass", got)
		}
		gate <- struct{}{}
		synctest.Wait()
		if got := in.most.Load(); got != 1 {
			t.Errorf("probes of one Binding ran %d at once, want 1", got)
		}
	})
}

func TestConcurrencyLimitHolds(t *testing.T) {
	g := &targets{}
	var ts []Target
	for i := range 8 {
		ts = append(ts, Target{Binding: fmt.Sprintf("b%d", i), Channel: fmt.Sprintf("C%d", i)})
	}
	g.set("r1", ts...)
	var in gauge
	var gate chan struct{}
	wait := func(context.Context, string) (Outcome, error) {
		defer in.enter()()
		<-gate
		return ok(), nil
	}
	agents, chans := &fakeAgents{fn: wait}, &fakeChannels{fn: wait}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans, Concurrency: 3, Timeout: time.Hour}
	}, func(_ *harness) {
		if got := in.cur.Load(); got != 3 {
			t.Fatalf("in flight = %d, want exactly the limit, 3", got)
		}
		close(gate)
		synctest.Wait()
		if got := in.most.Load(); got != 3 {
			t.Errorf("most probes at once = %d, want 3", got)
		}
		if a := agents.total(); a != 8 {
			t.Errorf("agent probes = %d, want 8", a)
		}
	})
}

func TestTargetsThatNameOneChannelEachGetTheirOwnProbeAndResult(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C1"}, Target{Binding: "c", Channel: "C1"}, Target{Binding: "d", Channel: "C2"})
	agents := &fakeAgents{}
	var n atomic.Int32
	chans := &fakeChannels{fn: func(context.Context, string) (Outcome, error) {
		return Outcome{Status: False, Reason: ReasonNotInChannel, Message: fmt.Sprintf("answer %d", n.Add(1))}, nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans}, func(h *harness) {
		if chans.count("C1") != 3 || chans.count("C2") != 1 {
			t.Errorf("channel probes C1=%d C2=%d, want one per Target (3 and 1)", chans.count("C1"), chans.count("C2"))
		}
		if agents.total() != 4 {
			t.Errorf("agent probes = %d, want one per Binding (4)", agents.total())
		}
		messages := map[string]bool{}
		for _, b := range []string{"a", "b", "c", "d"} {
			wantCond(t, h.conds(b), ChannelJoined, False, ReasonNotInChannel)
			wantCond(t, h.conds(b), Ready, False, "ChannelJoined:NotInChannel")
			messages[h.conds(b)[ChannelJoined].Message] = true
		}
		if len(messages) != 4 {
			t.Errorf("recorded %d distinct answers, want 4: each Binding records its own probe's result", len(messages))
		}
	})
}

func TestBindingsOfOneNameInTwoNamespacesAreKeptApart(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "sre/alerts", Channel: "C1"}, Target{Binding: "platform/alerts", Channel: "C2"})
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "platform/alerts" {
			return Outcome{Status: False, Reason: ReasonAgentNotFound}, nil
		}
		return ok(), nil
	}}
	var mu sync.Mutex
	var pruned []string
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, OnPrune: func(names []string) {
		mu.Lock()
		defer mu.Unlock()
		pruned = append(pruned, names...)
	}}, func(h *harness) {
		if agents.count("sre/alerts") != 1 || agents.count("platform/alerts") != 1 {
			t.Errorf("probes sre/alerts=%d platform/alerts=%d, want 1 each, under the full keys", agents.count("sre/alerts"), agents.count("platform/alerts"))
		}
		if n := len(h.p.State().Snapshot()); n != 2 {
			t.Fatalf("State holds %d Bindings, want 2", n)
		}
		wantCond(t, h.conds("sre/alerts"), BackendReachable, True, ReasonOK)
		wantCond(t, h.conds("platform/alerts"), BackendReachable, False, ReasonAgentNotFound)
		g.set("r2", Target{Binding: "sre/alerts", Channel: "C1"}) // only the platform Binding leaves
		h.advance(tick)
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(pruned, []string{"platform/alerts"}) || h.p.State().Snapshot()["sre/alerts"] == nil {
			t.Errorf("pruned %v, sre/alerts present %v; want only platform/alerts gone", pruned, h.p.State().Snapshot()["sre/alerts"] != nil)
		}
	})
}

func TestAgentAndChannelProbesAlternateSoHangingBackendsDoNotHoldChannelsBack(t *testing.T) {
	g := &targets{}
	var ts []Target
	for i := range 4 {
		ts = append(ts, Target{Binding: fmt.Sprintf("b%d", i), Channel: fmt.Sprintf("C%d", i)})
	}
	g.set("r1", ts...)
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done() // every backend hangs
		return Outcome{}, ctx.Err()
	}}
	chans := &fakeChannels{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans, Concurrency: 2}, func(_ *harness) {
		// Two slots, both on hanging backends or channels: with agent jobs first, no channel would have been
		// probed before the first two backends timed out.
		done := 0
		for i := range 4 {
			if chans.count(fmt.Sprintf("C%d", i)) > 0 {
				done++
			}
		}
		if done == 0 {
			t.Errorf("no channel was probed while the first backends hung")
		}
	})
}

func TestANewBindingIsPendingUntilItsFirstProbeFinishes(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var gate chan struct{}
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) { <-gate; return ok(), nil }}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: &fakeChannels{}, Timeout: time.Hour}
	}, func(h *harness) {
		c := h.conds("b")
		wantCond(t, c, BackendReachable, Unknown, ReasonPending)
		wantCond(t, c, ChannelJoined, True, ReasonOK)
		wantCond(t, c, Ready, Unknown, "BackendReachable:Pending")
		if !c[BackendReachable].LastProbeTime.IsZero() {
			t.Errorf("a Pending condition has LastProbeTime %v", c[BackendReachable].LastProbeTime)
		}

		close(gate)
		synctest.Wait()
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)
	})
}

func TestABindingAddedLaterIsProbedAtTheNextTick(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	agents, chans := &fakeAgents{}, &fakeChannels{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans}, func(h *harness) {
		g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"}) // same revision: only the Targets changed
		h.advance(tick)
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)
		if agents.count("b") != 1 || agents.count("a") != 2 {
			t.Errorf("agent probes a=%d b=%d; want 2 and 1", agents.count("a"), agents.count("b"))
		}
	})
}

func TestAChangedChannelIsProbedWhenTheRevisionChanges(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	chans := &fakeChannels{fn: func(_ context.Context, ch string) (Outcome, error) {
		if ch == "C2" {
			return Outcome{Status: False, Reason: ReasonChannelNotFound}, nil
		}
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: chans}, func(h *harness) {
		wantCond(t, h.conds("b"), ChannelJoined, True, ReasonOK)
		g.set("r2", Target{Binding: "b", Channel: "C2"})
		h.advance(tick)
		if chans.count("C2") != 1 {
			t.Fatalf("the new channel was probed %d times, want 1", chans.count("C2"))
		}
		wantCond(t, h.conds("b"), ChannelJoined, False, ReasonChannelNotFound)
	})
}

func TestOnlyTheCheckersGivenAreTracked(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Channels: &fakeChannels{}}, func(h *harness) {
		c := h.conds("b")
		if _, found := c[BackendReachable]; found {
			t.Error("BackendReachable is set without an AgentChecker")
		}
		wantCond(t, c, ChannelJoined, True, ReasonOK)
		wantCond(t, c, Ready, True, ReasonOK)
	})
}

func TestAProbeThatTimesOutLeavesTheConditionUnknown(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "slow", Channel: "C1"}, Target{Binding: "fast", Channel: "C2"})
	var hang atomic.Bool
	hang.Store(true)
	agents := &fakeAgents{fn: func(ctx context.Context, b string) (Outcome, error) {
		if b == "slow" && hang.Load() {
			<-ctx.Done()
			return Outcome{}, ctx.Err()
		}
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		wantCond(t, h.conds("fast"), BackendReachable, True, ReasonOK)
		wantCond(t, h.conds("slow"), BackendReachable, Unknown, ReasonPending)
		h.advance(timeout)
		c := h.conds("slow")
		wantCond(t, c, BackendReachable, Unknown, ReasonProbeTimeout)
		wantCond(t, c, Ready, Unknown, "BackendReachable:ProbeTimeout")
		wantCond(t, h.conds("fast"), BackendReachable, True, ReasonOK)

		// The loop is still running: the next interval probes both again, and the slow one has recovered.
		hang.Store(false)
		h.nextPass(agents.total)
		if agents.count("slow") != 2 || agents.count("fast") != 2 {
			t.Errorf("probes after a timeout: slow=%d fast=%d, want 2 each", agents.count("slow"), agents.count("fast"))
		}
		wantCond(t, h.conds("slow"), BackendReachable, True, ReasonOK)
	})
}

func TestACheckerThatAnswersTrueAfterItsContextEndedIsUnknown(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return ok(), nil // late, and not to be believed
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		h.advance(timeout)
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeTimeout)
	})
}

func TestACheckerThatIgnoresItsContextDoesNotStopTheProber(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "stuck", Channel: "C1"}, Target{Binding: "fine", Channel: "C2"})
	var gate chan struct{}
	var hang atomic.Bool
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "stuck" && hang.Load() {
			<-gate // never looks at its context
		}
		return ok(), nil
	}}
	// One probe at a time: the stuck backend must give up its place for "fine" to be probed in the same pass.
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Concurrency: 1}
	}, func(h *harness) {
		wantCond(t, h.conds("stuck"), Ready, True, ReasonOK)

		hang.Store(true)
		before := agents.count("fine")
		for agents.count("stuck") < 2 { // until the pass that hangs begins
			h.advance(tick)
		}
		h.advance(timeout)
		wantCond(t, h.conds("stuck"), BackendReachable, Unknown, ReasonProbeTimeout)
		wantCond(t, h.conds("stuck"), Ready, Unknown, "BackendReachable:ProbeTimeout") // not Ready=True
		if agents.count("fine") != before+1 {
			t.Errorf("a healthy Binding was probed %d times during the hung pass, want 1", agents.count("fine")-before)
		}

		// Later passes run, the healthy Binding keeps being probed, and the stuck target is not probed again.
		h.nextPass(func() int { return agents.count("fine") })
		if got := agents.count("stuck"); got != 2 {
			t.Errorf("the stuck target was probed %d times, want 2 (one of them still running)", got)
		}
		c := h.conds("stuck")[BackendReachable]
		if c.Status != Unknown || c.Reason != ReasonProbeTimeout {
			t.Errorf("stuck = %s/%s, want Unknown/ProbeTimeout", c.Status, c.Reason)
		}

		// When the checker returns, the target is probed again at the next pass and recovers.
		close(gate)
		synctest.Wait()
		h.nextPass(func() int { return agents.count("stuck") })
		wantCond(t, h.conds("stuck"), BackendReachable, True, ReasonOK)
		wantCond(t, h.conds("stuck"), Ready, True, ReasonOK)

		// A target that is stuck again when Run is cancelled does not hold Run back.
		gate = make(chan struct{})
		hang.Store(true)
		for agents.count("stuck") < 4 {
			h.advance(tick)
		}
		h.stop() // returns although the checker has not
		close(gate)
		synctest.Wait()
	})
}

func TestALateResultFromAStuckCheckerCannotBringBackAPrunedBinding(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "stuck", Channel: "C1"}, Target{Binding: "fine", Channel: "C2"})
	var gate chan struct{}
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "stuck" {
			<-gate
		}
		return ok(), nil
	}}
	var mu sync.Mutex
	var events []string
	log := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, s)
	}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{
			Targets: g.Targets, Revision: g.Revision, Agents: agents,
			OnTransition: func(tr Transition) { log("transition " + tr.Binding) },
			OnPrune:      func(names []string) { log("prune " + strings.Join(names, ",")) },
		}
	}, func(h *harness) {
		h.advance(timeout)
		wantCond(t, h.conds("stuck"), BackendReachable, Unknown, ReasonProbeTimeout)

		g.set("r2", Target{Binding: "fine", Channel: "C2"})
		h.advance(tick)
		if _, found := h.p.State().Snapshot()["stuck"]; found {
			t.Fatal("the Binding that left Targets is still in the State")
		}
		mu.Lock()
		n := len(events)
		mu.Unlock()

		close(gate) // the checker returns True, late
		synctest.Wait()
		h.advance(2 * interval)
		if _, found := h.p.State().Snapshot()["stuck"]; found {
			t.Error("a late result brought the pruned Binding back")
		}
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events[n:] {
			if strings.Contains(e, "stuck") {
				t.Errorf("event %q for a pruned Binding", e)
			}
		}
	})
}

func TestACheckerThatReportsAnUnusableReasonIsNotBelieved(t *testing.T) {
	const leak = "token abc123 rejected"
	cases := map[string]Outcome{
		"true with a failure": {Status: True, Reason: ReasonNotInChannel},
		"false with ok":       {Status: False, Reason: ReasonOK},
		"unknown with ok":     {Status: Unknown, Reason: ReasonOK},
		"outside the set":     {Status: False, Reason: Reason(leak)},
		"pending":             {Status: Unknown, Reason: ReasonPending},
		"a probe timeout":     {Status: False, Reason: ReasonProbeTimeout},
		"a probe failure":     {Status: Unknown, Reason: ReasonProbeFailed},
		"unavailable false":   {Status: False, Reason: ReasonChatUnavailable},
		"rejected unknown":    {Status: Unknown, Reason: ReasonChatCredentialsRejected},
		"text as a status":    {Status: Status(leak), Reason: ReasonAgentNotFound},
	}
	g := &targets{}
	var ts []Target
	for b := range cases {
		ts = append(ts, Target{Binding: b, Channel: "C1"})
	}
	g.set("r1", ts...)
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) { return cases[b], nil }}
	var mu sync.Mutex
	got := map[string]error{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, OnProbeError: func(pe ProbeError) {
		mu.Lock()
		defer mu.Unlock()
		got[pe.Target.Binding] = pe.Err
	}}, func(h *harness) {
		mu.Lock()
		defer mu.Unlock()
		for b := range cases {
			conds := h.conds(b)
			wantCond(t, conds, BackendReachable, Unknown, ReasonProbeFailed)
			wantCond(t, conds, Ready, Unknown, "BackendReachable:ProbeFailed")
			if !conds[BackendReachable].LastProbeTime.After(time.Time{}) {
				t.Errorf("%s: LastProbeTime is zero, but a probe ran", b)
			}
			for _, typ := range []ConditionType{BackendReachable, Ready} {
				if strings.Contains(conds[typ].Message, leak) {
					t.Errorf("%s: %s message %q carries the checker's text", b, typ, conds[typ].Message)
				}
			}
			if got[b] == nil {
				t.Errorf("%s: no ProbeError", b)
			} else if strings.Contains(got[b].Error(), leak) {
				t.Errorf("%s: ProbeError %q carries the checker's text", b, got[b])
			}
		}
	})
}

func TestEveryReasonACheckerMayReportIsReportable(t *testing.T) {
	for _, r := range reasonConstants(t) {
		var want []Status
		switch r {
		case ReasonOK:
			want = []Status{True}
		case ReasonPending, ReasonProbeFailed, ReasonProbeTimeout:
		case ReasonChatCredentialsRejected:
			want = []Status{False}
		case ReasonChatUnavailable, ReasonChatCredentialsChanged, ReasonChatCredentialsUnusable:
			want = []Status{Unknown}
		default:
			want = []Status{False, Unknown}
		}
		for _, s := range []Status{True, False, Unknown} {
			if got := r.reportable(s); got != slices.Contains(want, s) {
				t.Errorf("%s.reportable(%s) = %v, want %v", r, s, got, !got)
			}
		}
	}
}

func TestACheckerThatFailsLeavesTheConditionUnknownAndTheOthersRun(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "errs", Channel: "C1"}, Target{Binding: "panics", Channel: "C2"}, Target{Binding: "garbage", Channel: "C3"}, Target{Binding: "fine", Channel: "C4"})
	cause := errors.New("token abc123 rejected")
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		switch b {
		case "errs":
			// A true outcome beside an error is not believed.
			return Outcome{Status: True, Reason: ReasonOK}, cause
		case "panics":
			panic("secret-token")
		case "garbage":
			return Outcome{Status: "Maybe", Reason: ReasonAgentNotFound}, nil
		}
		return ok(), nil
	}}
	chans := &fakeChannels{fn: func(context.Context, string) (Outcome, error) { return Outcome{}, errors.New("slack down") }}
	var mu sync.Mutex
	var perrs []ProbeError
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans, OnProbeError: func(pe ProbeError) {
		mu.Lock()
		defer mu.Unlock()
		perrs = append(perrs, pe)
	}}, func(h *harness) {
		for _, b := range []string{"errs", "panics", "garbage"} {
			wantCond(t, h.conds(b), BackendReachable, Unknown, ReasonProbeFailed)
			wantCond(t, h.conds(b), Ready, Unknown, "BackendReachable:ProbeFailed")
		}
		wantCond(t, h.conds("fine"), BackendReachable, True, ReasonOK)
		wantCond(t, h.conds("fine"), ChannelJoined, Unknown, ReasonProbeFailed)
		for b, cs := range h.p.State().Snapshot() {
			for _, c := range cs {
				for _, leak := range []string{"abc123", "secret-token", "slack down"} {
					if strings.Contains(c.Message, leak) {
						t.Errorf("%s %s message %q leaks checker error text", b, c.Type, c.Message)
					}
				}
			}
		}

		mu.Lock()
		defer mu.Unlock()
		byBinding := map[string]ProbeError{}
		channelErrs := 0
		for _, pe := range perrs {
			if pe.Condition == ChannelJoined {
				channelErrs++
				if pe.Target.Binding == "" || pe.Err == nil || pe.Err.Error() != "slack down" {
					t.Errorf("channel ProbeError = %+v", pe)
				}
				continue
			}
			if pe.Target.Binding == "" || pe.Condition != BackendReachable {
				t.Errorf("ProbeError = %+v, want a BackendReachable target", pe)
				continue
			}
			byBinding[pe.Target.Binding] = pe
		}
		if channelErrs != 4 {
			t.Errorf("channel ProbeErrors = %d, want 4", channelErrs)
		}
		if pe := byBinding["errs"]; !errors.Is(pe.Err, cause) || pe.Target.Channel != "C1" {
			t.Errorf("errs ProbeError = %+v, want the checker's error and the Binding's channel", pe)
		}
		var panicked *PanicError
		if pe := byBinding["panics"]; !errors.As(pe.Err, &panicked) || panicked.Value != "secret-token" || !strings.Contains(string(panicked.Stack), "prober_test.go") {
			t.Errorf("panics ProbeError = %+v, want a PanicError with the value and a stack naming the checker", pe)
		}
		if pe := byBinding["garbage"]; pe.Err == nil {
			t.Error("garbage status gave no ProbeError")
		}
		if _, found := byBinding["fine"]; found {
			t.Error("a healthy probe gave a ProbeError")
		}
	})
}

func TestAnOutcomeWithNoReasonIsTrueOrAFault(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C1"}, Target{Binding: "c", Channel: "C1"})
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		return Outcome{Status: map[string]Status{"a": True, "b": False, "c": Unknown}[b]}, nil
	}}
	var faults atomic.Int32
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, OnProbeError: func(ProbeError) { faults.Add(1) }}, func(h *harness) {
		wantCond(t, h.conds("a"), BackendReachable, True, ReasonOK)
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeFailed)
		wantCond(t, h.conds("c"), BackendReachable, Unknown, ReasonProbeFailed)
		if faults.Load() != 2 {
			t.Errorf("ProbeErrors = %d, want 2 (the two with no reason)", faults.Load())
		}
	})
}

func TestPruneDropsABindingThatLeftTargetsAndSilencesItsHook(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "keep", Channel: "C1"}, Target{Binding: "gone", Channel: "C2"})
	var mu sync.Mutex
	var events []string
	var flip atomic.Bool
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "gone" && flip.Load() {
			return Outcome{Status: False, Reason: ReasonUnreachable}, nil
		}
		return ok(), nil
	}}
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: agents,
		OnTransition: func(tr Transition) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, "transition "+tr.Binding)
		},
		OnPrune: func(names []string) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, "prune "+strings.Join(names, ","))
		},
	}, func(h *harness) {
		if len(h.p.State().Snapshot()) != 2 {
			t.Fatalf("snapshot = %+v", h.p.State().Snapshot())
		}
		flip.Store(true) // were "gone" still probed, its condition would now change
		g.set("r2", Target{Binding: "keep", Channel: "C1"})
		h.advance(tick)
		if snap := h.p.State().Snapshot(); len(snap) != 1 || snap["keep"] == nil {
			t.Fatalf("after the Binding left Targets: %+v", snap)
		}
		h.advance(2 * interval)
		mu.Lock()
		defer mu.Unlock()
		prune := slices.Index(events, "prune gone")
		if prune < 0 {
			t.Fatalf("no prune event in %v", events)
		}
		if n := slices.Index(events, "transition gone"); n < 0 || n > prune {
			t.Errorf("the transitions for gone must come before its prune: %v", events)
		}
		if last := slices.Index(events[prune+1:], "transition gone"); last >= 0 {
			t.Errorf("a transition for gone came after its prune: %v", events)
		}
		if n := len(events) - len(slices.DeleteFunc(slices.Clone(events), func(e string) bool { return e == "prune gone" })); n != 1 {
			t.Errorf("prune reported more than once: %v", events)
		}
		if agents.count("gone") != 1 {
			t.Errorf("a pruned Binding was probed %d times, want 1", agents.count("gone"))
		}
	})
}

func TestOnTransitionFiresOnlyOnAChangeOfStatusOrReason(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	var mu sync.Mutex
	var events []string
	var step atomic.Int32
	chans := &fakeChannels{fn: func(context.Context, string) (Outcome, error) {
		switch step.Load() {
		case 0, 1: // True, True
			return ok(), nil
		case 2, 3: // False/NotInChannel twice
			return Outcome{Status: False, Reason: ReasonNotInChannel}, nil
		case 4: // another reason
			return Outcome{Status: False, Reason: ReasonChannelArchived}, nil
		}
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: chans, OnTransition: func(tr Transition) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, fmt.Sprintf("%s:%s/%s>%s/%s", tr.New.Type, tr.Old.Status, tr.Old.Reason, tr.New.Status, tr.New.Reason))
	}}, func(h *harness) {
		of := func(typ ConditionType) []string {
			mu.Lock()
			defer mu.Unlock()
			var out []string
			for _, e := range events {
				if strings.HasPrefix(e, string(typ)+":") {
					out = append(out, e)
				}
			}
			return out
		}
		// Pending is not a transition; the first probes are.
		if got := of(BackendReachable); len(got) != 1 {
			t.Fatalf("BackendReachable transitions after the first pass = %v, want 1", got)
		}
		for pass := 1; pass <= 5; pass++ {
			step.Store(int32(pass))
			h.nextPass(func() int { return chans.count("C1") })
		}
		want := []string{
			"ChannelJoined:Unknown/Pending>True/OK",
			"ChannelJoined:True/OK>False/NotInChannel",
			"ChannelJoined:False/NotInChannel>False/ChannelArchived",
			"ChannelJoined:False/ChannelArchived>True/OK",
		}
		if got := of(ChannelJoined); !slices.Equal(got, want) {
			t.Errorf("ChannelJoined transitions = %v\nwant %v", got, want)
		}
		if got := of(BackendReachable); len(got) != 1 {
			t.Errorf("BackendReachable fired %d times across six passes, want 1", len(got))
		}
		ready := of(Ready)
		if last := ready[len(ready)-1]; last != "Ready:False/ChannelJoined:ChannelArchived>True/OK" {
			t.Errorf("last Ready transition = %q", last)
		}
	})
}

func TestHookCallsAreSerializedAcrossBindings(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"}, Target{Binding: "c", Channel: "C3"})
	var in gauge
	var release chan struct{}
	runIn(t, func() Options {
		release = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Concurrency: 3, OnTransition: func(Transition) {
			defer in.enter()()
			<-release
		}}
	}, func(_ *harness) {
		// Three probes finished at once and each wants to report: one is in the hook, the others wait for it.
		if got := in.cur.Load(); got != 1 {
			t.Fatalf("hook calls in progress = %d, want 1", got)
		}
		for range 6 { // BackendReachable and Ready for each of three Bindings
			release <- struct{}{}
		}
		synctest.Wait()
		if got := in.most.Load(); got != 1 {
			t.Errorf("hook calls overlapped: %d at once", got)
		}
	})
}

func TestResultsWaitingForTheHookAreDroppedAfterCancel(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"}, Target{Binding: "c", Channel: "C3"})
	var hooks atomic.Int32
	var release chan struct{}
	runIn(t, func() Options {
		release = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Concurrency: 3, OnTransition: func(Transition) {
			hooks.Add(1)
			<-release
		}}
	}, func(h *harness) {
		defer close(release) // a hook that a failed run left waiting must not hang the bubble
		// Three probes have finished; one is in the hook and the other two wait to record.
		if got := hooks.Load(); got != 1 {
			t.Fatalf("hook calls before the cancel = %d, want 1: the result that is recording", got)
		}
		h.cancel()
		synctest.Wait()
		// Let the one in the hook finish: BackendReachable, then Ready, of the first Binding. A result that still
		// waited to record would take its place and call the hook a third time.
		release <- struct{}{}
		release <- struct{}{}
		synctest.Wait()
		if got := hooks.Load(); got != 2 {
			t.Errorf("hook calls = %d, want 2: only the result that was recording when the cancel came", got)
		}
		pending := 0
		for _, b := range []string{"a", "b", "c"} {
			if h.conds(b)[BackendReachable].Reason == ReasonPending {
				pending++
			}
		}
		if pending != 2 {
			t.Errorf("%d Bindings still Pending, want 2", pending)
		}
	})
}

func TestACheckerThatAnswersAtTheDeadlineIsUnknown(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		time.Sleep(timeout) // wakes at the very instant the probe's deadline passes
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		h.advance(timeout)
		wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeTimeout)
	})
}

func TestShutdownRecordsNothingForProbesStillInFlight(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
	var hooks atomic.Int32
	started := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return ok(), nil // a cancelled shutdown is not a result, whatever it says
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: started, Channels: &fakeChannels{fn: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}}, OnTransition: func(Transition) { hooks.Add(1) }, OnProbeError: func(ProbeError) { hooks.Add(1) }}, func(h *harness) {
		if started.total() != 2 {
			t.Fatalf("probes in flight = %d, want 2", started.total())
		}
		h.stop()
		for _, b := range []string{"a", "b"} {
			c := h.conds(b)
			wantCond(t, c, BackendReachable, Unknown, ReasonPending)
			wantCond(t, c, ChannelJoined, Unknown, ReasonPending)
			wantCond(t, c, Ready, Unknown, "BackendReachable:Pending")
		}
		if hooks.Load() != 0 {
			t.Errorf("hooks fired %d times for probes cut off by shutdown", hooks.Load())
		}
	})
}

func TestNoProbeStartsAfterCancel(t *testing.T) {
	g := &targets{}
	var ts []Target
	for i := range 5 {
		ts = append(ts, Target{Binding: fmt.Sprintf("b%d", i), Channel: fmt.Sprintf("C%d", i)})
	}
	g.set("r1", ts...)
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return ok(), nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Concurrency: 1}, func(h *harness) {
		if agents.total() != 1 {
			t.Fatalf("probes started = %d, want 1 (one at a time)", agents.total())
		}
		h.stop()
		if got := agents.total(); got != 1 {
			t.Errorf("%d probes started, %d of them after the cancel", got, got-1)
		}
	})
}

func TestRunReturnsOnCancelWhenIdleAndLeavesNoGoroutine(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	// The bubble fails the test if a goroutine of the Prober outlives the body.
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}}, func(h *harness) {
		h.advance(tick)
		h.stop()
	})
}

func TestRunWithNoTargetsOrRevisionIsHarmless(t *testing.T) {
	run(t, Options{}, func(h *harness) {
		h.advance(tick)
		if n := len(h.p.State().Snapshot()); n != 0 {
			t.Errorf("snapshot has %d Bindings", n)
		}
	})
}
