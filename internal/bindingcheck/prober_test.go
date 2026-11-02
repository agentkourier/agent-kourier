package bindingcheck

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tick     = 5 * time.Second
	timeout  = 10 * time.Second
	interval = time.Minute
)

// fakeClock is a clock that moves only when a test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	at time.Time
	d  time.Duration
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{now: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, &waiter{at: c.now.Add(d), d: d, ch: ch})
	return ch
}

// Advance moves the clock and fires every wait that has come due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.waiters = slices.DeleteFunc(c.waiters, func(w *waiter) bool {
		if w.at.After(c.now) {
			return false
		}
		w.ch <- c.now
		return true
	})
}

// pending is how many waits of length d have not fired.
func (c *fakeClock) pending(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, w := range c.waiters {
		if w.d == d {
			n++
		}
	}
	return n
}

// eventually polls cond until it holds, and fails the test if it does not within five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

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

func (f *fakeChannels) CheckChannel(ctx context.Context, ch string) (Outcome, error) {
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

// harness runs a Prober on a fake clock for the length of a test.
type harness struct {
	t      *testing.T
	clock  *fakeClock
	p      *Prober
	cancel context.CancelFunc
	done   chan struct{}
}

// start runs a Prober with o on a fake clock. The jitter is fixed at exactly Interval unless o.Rand is set.
func start(t *testing.T, o Options) *harness {
	t.Helper()
	h := &harness{t: t, clock: newClock(), done: make(chan struct{})}
	o.Now, o.After = h.clock.Now, h.clock.After
	if o.Rand == nil {
		o.Rand = func() float64 { return 0.5 }
	}
	if o.Interval == 0 {
		o.Interval = interval
	}
	o.Tick = tick
	if o.Timeout == 0 {
		o.Timeout = timeout
	}
	h.p = NewProber(o)
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	go func() { defer close(h.done); h.p.Run(ctx) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("Run did not return after its context ended")
	}
}

// idle waits until the loop is waiting for its next Tick, which is when a pass has finished.
func (h *harness) idle() {
	h.t.Helper()
	eventually(h.t, "the loop to wait for a tick", func() bool { return h.clock.pending(tick) == 1 })
}

// step moves the clock by one Tick, and returns when the loop (and any pass it began) is done with it.
func (h *harness) step() {
	h.t.Helper()
	h.idle()
	h.clock.Advance(tick)
	h.idle()
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
		want time.Duration
	}{{0, lo}, {0.5, iv}, {math.Nextafter(1, 0), hi}}
	for _, tt := range tests {
		got := jittered(iv, tt.r)
		if tt.r == 0.5 || tt.r == 0 {
			if got != tt.want {
				t.Errorf("jittered(r=%v) = %v, want %v", tt.r, got, tt.want)
			}
		}
		if got < lo || got > hi {
			t.Errorf("jittered(r=%v) = %v, outside [%v, %v]", tt.r, got, lo, hi)
		}
	}
	// The default source stays in bounds too, and actually varies.
	p := NewProber(Options{})
	seen := map[time.Duration]bool{}
	for range 1000 {
		got := jittered(p.o.Interval, p.o.Rand())
		if got < p.o.Interval*9/10 || got > p.o.Interval*11/10 {
			t.Fatalf("default jitter gave %v for an interval of %v", got, p.o.Interval)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Errorf("default jitter gave only %d distinct values in 1000 draws", len(seen))
	}
}

func TestNewProberDefaults(t *testing.T) {
	o := NewProber(Options{}).o
	if o.Interval != 5*time.Minute || o.Tick != 5*time.Second || o.Timeout != 10*time.Second || o.Concurrency != 4 {
		t.Errorf("defaults = interval %v tick %v timeout %v concurrency %d", o.Interval, o.Tick, o.Timeout, o.Concurrency)
	}
	if o.Now == nil || o.After == nil || o.Rand == nil {
		t.Error("clock or random source not defaulted")
	}
}

func TestPassUsesTheJitteredInterval(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	agents := &fakeAgents{}
	// r = 0 makes the pause 90% of a minute: 54s, which is reached at the eleventh 5s tick (55s), not the tenth.
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Rand: func() float64 { return 0 }})
	h.idle()
	for range 10 {
		h.step()
	}
	if got := agents.count("b"); got != 1 {
		t.Fatalf("probes after 50s = %d, want 1", got)
	}
	h.step()
	if got := agents.count("b"); got != 2 {
		t.Errorf("probes after 55s = %d, want 2 (the pause was 54s)", got)
	}
}

func TestProbesAtStartAndOnTheInterval(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	agents, chans := &fakeAgents{}, &fakeChannels{}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans})
	h.idle()
	if agents.count("b") != 1 || chans.count("C1") != 1 {
		t.Fatalf("probes at start = %d agent, %d channel; want 1 each", agents.count("b"), chans.count("C1"))
	}
	c := h.conds("b")
	wantCond(t, c, BackendReachable, True, ReasonOK)
	wantCond(t, c, ChannelJoined, True, ReasonOK)
	wantCond(t, c, Ready, True, ReasonOK)

	for range 11 { // 55s
		h.step()
	}
	if agents.count("b") != 1 {
		t.Fatalf("probed again after 55s of a 60s interval")
	}
	h.step() // 60s
	if agents.count("b") != 2 || chans.count("C1") != 2 {
		t.Errorf("probes at the interval = %d agent, %d channel; want 2 each", agents.count("b"), chans.count("C1"))
	}
	if got := h.conds("b")[BackendReachable].LastProbeTime; !got.Equal(t0.Add(interval)) {
		t.Errorf("LastProbeTime = %v, want %v", got, t0.Add(interval))
	}
}

func TestUnchangedRevisionDoesNotProbeEarlyAndChangedOneDoesAtTheNextTick(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	agents := &fakeAgents{}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents})
	h.idle()
	for range 3 {
		h.step()
	}
	if got := agents.count("b"); got != 1 {
		t.Fatalf("probes with an unchanged revision = %d, want 1", got)
	}

	g.set("r2", Target{"b", "C1"})
	h.idle()
	if got := agents.count("b"); got != 1 {
		t.Fatalf("the probe ran before the tick: %d", got)
	}
	h.step()
	if got := agents.count("b"); got != 2 {
		t.Errorf("probes after a revision change = %d, want 2", got)
	}
	h.step()
	if got := agents.count("b"); got != 2 {
		t.Errorf("probes after the revision settled = %d, want 2", got)
	}
}

func TestPassesDoNotOverlap(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	var inflight, most atomic.Int32
	gate := make(chan struct{})
	agents := &fakeAgents{fn: func(_ context.Context, _ string) (Outcome, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-gate
		return ok(), nil
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Concurrency: 4, Timeout: time.Hour})
	eventually(t, "the first probe", func() bool { return inflight.Load() == 1 })

	// The revision changes and the interval runs out while the pass is still going.
	g.set("r2", Target{"b", "C1"})
	h.clock.Advance(2 * interval)
	time.Sleep(20 * time.Millisecond)
	if got := agents.total(); got != 1 {
		t.Fatalf("a second pass began during the first: %d probes", got)
	}
	gate <- struct{}{}
	h.idle()
	h.clock.Advance(tick) // the next tick sees the new revision
	eventually(t, "the second pass", func() bool { return agents.total() == 2 })
	gate <- struct{}{}
	h.idle()
	if got := most.Load(); got != 1 {
		t.Errorf("probes of one Binding ran %d at once, want 1", got)
	}
}

func TestConcurrencyLimitHolds(t *testing.T) {
	g := &targets{}
	var ts []Target
	for i := range 8 {
		ts = append(ts, Target{fmt.Sprintf("b%d", i), fmt.Sprintf("C%d", i)})
	}
	g.set("r1", ts...)
	var inflight, most atomic.Int32
	gate := make(chan struct{})
	track := func() func() {
		n := inflight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		return func() { inflight.Add(-1) }
	}
	agents := &fakeAgents{fn: func(_ context.Context, _ string) (Outcome, error) {
		defer track()()
		<-gate
		return ok(), nil
	}}
	chans := &fakeChannels{fn: func(_ context.Context, _ string) (Outcome, error) {
		defer track()()
		<-gate
		return ok(), nil
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans, Concurrency: 3})
	eventually(t, "three probes in flight", func() bool { return inflight.Load() == 3 })
	time.Sleep(20 * time.Millisecond) // a fourth, if the limit leaked, would have started by now
	if got := inflight.Load(); got != 3 {
		t.Fatalf("in flight = %d, want 3", got)
	}
	close(gate)
	h.idle()
	if got := most.Load(); got != 3 {
		t.Errorf("most probes at once = %d, want 3", got)
	}
	if got := agents.total(); got != 8 {
		t.Errorf("agent probes = %d, want 8", got)
	}
}

func TestChannelProbesAreDeduplicatedByChannel(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"a", "C1"}, Target{"b", "C1"}, Target{"c", "C1"}, Target{"d", "C2"})
	agents := &fakeAgents{}
	chans := &fakeChannels{fn: func(_ context.Context, ch string) (Outcome, error) {
		if ch == "C1" {
			return Outcome{Status: False, Reason: ReasonNotInChannel, Message: "not a member"}, nil
		}
		return ok(), nil
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans})
	h.idle()
	if chans.count("C1") != 1 || chans.count("C2") != 1 {
		t.Errorf("channel probes C1=%d C2=%d, want 1 each", chans.count("C1"), chans.count("C2"))
	}
	if agents.total() != 4 {
		t.Errorf("agent probes = %d, want one per Binding (4)", agents.total())
	}
	for _, b := range []string{"a", "b", "c"} {
		wantCond(t, h.conds(b), ChannelJoined, False, ReasonNotInChannel)
		wantCond(t, h.conds(b), Ready, False, "ChannelJoined/NotInChannel")
	}
	wantCond(t, h.conds("d"), ChannelJoined, True, ReasonOK)
}

func TestANewBindingIsPendingUntilItsFirstProbeFinishes(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	gate := make(chan struct{})
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) { <-gate; return ok(), nil }}
	chans := &fakeChannels{}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans})
	eventually(t, "the agent probe to start", func() bool { return agents.total() == 1 })
	eventually(t, "the channel probe", func() bool { return h.conds("b")[ChannelJoined].Status == True })
	c := h.conds("b")
	wantCond(t, c, BackendReachable, Unknown, ReasonPending)
	wantCond(t, c, Ready, Unknown, "BackendReachable/Pending")

	close(gate)
	h.idle()
	wantCond(t, h.conds("b"), Ready, True, ReasonOK)
}

func TestABindingAddedLaterIsProbedAtTheNextTick(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"a", "C1"})
	agents, chans := &fakeAgents{}, &fakeChannels{}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans})
	h.idle()

	g.set("r1", Target{"a", "C1"}, Target{"b", "C2"}) // same revision: only the Targets changed
	h.step()
	wantCond(t, h.conds("b"), Ready, True, ReasonOK)
	if agents.count("b") != 1 || agents.count("a") != 2 {
		t.Errorf("agent probes a=%d b=%d; want 2 and 1", agents.count("a"), agents.count("b"))
	}
}

func TestAMovedBindingGoesPendingForItsChannelThenIsProbed(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{fn: func(_ context.Context, ch string) (Outcome, error) {
		if ch == "C2" {
			return Outcome{Status: False, Reason: ReasonChannelNotFound}, nil
		}
		return ok(), nil
	}}})
	h.idle()
	wantCond(t, h.conds("b"), ChannelJoined, True, ReasonOK)
	g.set("r2", Target{"b", "C2"})
	h.step()
	wantCond(t, h.conds("b"), ChannelJoined, False, ReasonChannelNotFound)
}

func TestOnlyTheCheckersGivenAreTracked(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Channels: &fakeChannels{}})
	h.idle()
	c := h.conds("b")
	if _, found := c[BackendReachable]; found {
		t.Error("BackendReachable is set without an AgentChecker")
	}
	wantCond(t, c, ChannelJoined, True, ReasonOK)
	wantCond(t, c, Ready, True, ReasonOK)
}

func TestAProbeThatTimesOutLeavesTheConditionUnknown(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"slow", "C1"}, Target{"fast", "C2"})
	var hang atomic.Bool
	hang.Store(true)
	agents := &fakeAgents{fn: func(ctx context.Context, b string) (Outcome, error) {
		if b == "slow" && hang.Load() {
			<-ctx.Done()
			return Outcome{}, ctx.Err()
		}
		return ok(), nil
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents})
	eventually(t, "the fast probe", func() bool { return h.conds("fast")[BackendReachable].Status == True })
	h.clock.Advance(timeout)
	h.idle()
	c := h.conds("slow")
	wantCond(t, c, BackendReachable, Unknown, ReasonProbeTimeout)
	wantCond(t, c, Ready, Unknown, "BackendReachable/ProbeTimeout")
	wantCond(t, h.conds("fast"), BackendReachable, True, ReasonOK)

	// The loop is still running: the next interval probes both again, and the slow one has recovered.
	hang.Store(false)
	for range 12 {
		h.step()
	}
	if agents.count("slow") != 2 || agents.count("fast") != 2 {
		t.Errorf("probes after a timeout: slow=%d fast=%d, want 2 each", agents.count("slow"), agents.count("fast"))
	}
	wantCond(t, h.conds("slow"), BackendReachable, True, ReasonOK)
}

func TestACheckerThatIgnoresItsTimeoutAndSaysTrueIsStillUnknown(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return ok(), nil // late, and not to be believed
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents})
	eventually(t, "the probe to start", func() bool { return agents.total() == 1 })
	h.clock.Advance(timeout)
	h.idle()
	wantCond(t, h.conds("b"), BackendReachable, Unknown, ReasonProbeTimeout)
}

func TestACheckerThatFailsLeavesTheConditionUnknownAndTheOthersRun(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"errs", "C1"}, Target{"panics", "C2"}, Target{"garbage", "C3"}, Target{"fine", "C4"})
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		switch b {
		case "errs":
			// A true outcome beside an error is not believed.
			return Outcome{Status: True, Reason: ReasonOK}, errors.New("token abc123 rejected")
		case "panics":
			panic("secret-token")
		case "garbage":
			return Outcome{Status: "Maybe"}, nil
		}
		return ok(), nil
	}}
	chans := &fakeChannels{fn: func(context.Context, string) (Outcome, error) { return Outcome{}, errors.New("slack down") }}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans})
	h.idle()
	for _, b := range []string{"errs", "panics", "garbage"} {
		wantCond(t, h.conds(b), BackendReachable, Unknown, ReasonProbeFailed)
		wantCond(t, h.conds(b), Ready, Unknown, "BackendReachable/ProbeFailed")
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
}

func TestAnOutcomeWithNoReasonGetsOne(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"a", "C1"}, Target{"b", "C1"})
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "a" {
			return Outcome{Status: True}, nil
		}
		return Outcome{Status: False}, nil
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents})
	h.idle()
	wantCond(t, h.conds("a"), BackendReachable, True, ReasonOK)
	wantCond(t, h.conds("b"), BackendReachable, False, ReasonProbeFailed)
}

func TestPruneDropsABindingThatLeftTargetsAndSilencesItsHook(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"keep", "C1"}, Target{"gone", "C2"})
	var mu sync.Mutex
	hooks := map[string]int{}
	var flip atomic.Bool
	agents := &fakeAgents{fn: func(_ context.Context, b string) (Outcome, error) {
		if b == "gone" && flip.Load() {
			return Outcome{Status: False, Reason: ReasonUnreachable}, nil
		}
		return ok(), nil
	}}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, OnTransition: func(tr Transition) {
		mu.Lock()
		defer mu.Unlock()
		hooks[tr.Binding]++
	}})
	h.idle()
	if len(h.p.State().Snapshot()) != 2 {
		t.Fatalf("snapshot = %+v", h.p.State().Snapshot())
	}
	mu.Lock()
	before := hooks["gone"]
	mu.Unlock()

	flip.Store(true) // were "gone" still probed, its condition would now change
	g.set("r2", Target{"keep", "C1"})
	h.step()
	if snap := h.p.State().Snapshot(); len(snap) != 1 || snap["keep"] == nil {
		t.Fatalf("after the Binding left Targets: %+v", snap)
	}
	for range 12 { // a full interval
		h.step()
	}
	mu.Lock()
	defer mu.Unlock()
	if hooks["gone"] != before {
		t.Errorf("hook fired %d times for a pruned Binding", hooks["gone"]-before)
	}
	if agents.count("gone") != 1 {
		t.Errorf("a pruned Binding was probed %d times, want 1", agents.count("gone"))
	}
}

func TestOnTransitionFiresOnlyOnAChangeOfStatusOrReason(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{"b", "C1"})
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
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: chans, OnTransition: func(tr Transition) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, fmt.Sprintf("%s:%s/%s>%s/%s", tr.New.Type, tr.Old.Status, tr.Old.Reason, tr.New.Status, tr.New.Reason))
	}})
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
	h.idle()
	// Pending is not a transition; the first probes are. BackendReachable lands first or second, so Ready may change
	// reason on the way (BackendReachable/Pending to ChannelJoined/Pending); the end state is what counts for it.
	count := func(typ ConditionType) int {
		n := 0
		for _, e := range got() {
			if strings.HasPrefix(e, string(typ)+":") {
				n++
			}
		}
		return n
	}
	if n := count(BackendReachable); n != 1 {
		t.Fatalf("BackendReachable transitions after the first pass = %d (%v), want 1", n, got())
	}
	if n := count(ChannelJoined); n != 1 {
		t.Fatalf("ChannelJoined transitions after the first pass = %d (%v), want 1", n, got())
	}

	// Each later pass is one interval of 12 ticks.
	for pass := 1; pass <= 5; pass++ {
		step.Store(int32(pass))
		for range 12 {
			h.step()
		}
	}
	want := []string{
		"ChannelJoined:Unknown/Pending>True/OK",
		"ChannelJoined:True/OK>False/NotInChannel",
		"ChannelJoined:False/NotInChannel>False/ChannelArchived",
		"ChannelJoined:False/ChannelArchived>True/OK",
	}
	var channel []string
	for _, e := range got() {
		if strings.HasPrefix(e, "ChannelJoined:") {
			channel = append(channel, e)
		}
	}
	if !slices.Equal(channel, want) {
		t.Errorf("ChannelJoined transitions = %v\nwant %v", channel, want)
	}
	if n := count(BackendReachable); n != 1 {
		t.Errorf("BackendReachable fired %d times across six passes, want 1", n)
	}
	readyLast := ""
	for _, e := range got() {
		if strings.HasPrefix(e, "Ready:") {
			readyLast = e
		}
	}
	if readyLast != "Ready:False/ChannelJoined/ChannelArchived>True/OK" {
		t.Errorf("last Ready transition = %q", readyLast)
	}
}

func TestRunReturnsOnCancelWithoutLeakingGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	g := &targets{}
	g.set("r1", Target{"a", "C1"}, Target{"b", "C2"}, Target{"c", "C3"})
	started := make(chan struct{}, 8)
	block := func(ctx context.Context) (Outcome, error) {
		started <- struct{}{}
		<-ctx.Done()
		return ok(), nil // a cancelled shutdown is not a result, whatever it says
	}
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) { return block(ctx) }}
	chans := &fakeChannels{fn: func(ctx context.Context, _ string) (Outcome, error) { return block(ctx) }}
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans, Concurrency: 4})
	for range 4 {
		<-started
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return while probes were in flight")
	}
	eventually(t, "goroutines to finish", func() bool { return runtime.NumGoroutine() <= base })
	for _, c := range h.p.State().Snapshot()["a"] {
		if c.Status == True {
			t.Errorf("a result from a cancelled probe was recorded: %+v", c)
		}
	}
}

func TestRunReturnsOnCancelWhenIdle(t *testing.T) {
	base := runtime.NumGoroutine()
	g := &targets{}
	g.set("r1", Target{"a", "C1"})
	h := start(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}})
	h.idle()
	h.stop()
	eventually(t, "goroutines to finish", func() bool { return runtime.NumGoroutine() <= base })
}

func TestRunWithNoTargetsOrRevisionIsHarmless(t *testing.T) {
	h := start(t, Options{})
	h.idle()
	h.step()
	if n := len(h.p.State().Snapshot()); n != 0 {
		t.Errorf("snapshot has %d Bindings", n)
	}
}
