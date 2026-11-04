package bindingcheck

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Ready is as old as the oldest evidence it is derived from: a Ready that said it had observed generation 3 while a probe of
// generation 2 held it back would claim more than was seen.
func TestReadyObservesTheOldestGenerationItIsDerivedFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		a, c int64
		want int64
	}{
		"equal":              {3, 3, 3},
		"channel is older":   {3, 2, 2},
		"backend is older":   {1, 4, 1},
		"files have none":    {0, 0, 0},
		"one has none (old)": {0, 3, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewState()
			s.Set("b", t0,
				Condition{Type: BackendReachable, Status: True, Reason: ReasonOK, ObservedGeneration: tc.a},
				Condition{Type: ChannelJoined, Status: True, Reason: ReasonOK, ObservedGeneration: tc.c})
			if got := find(t, s.Snapshot()["b"], Ready).ObservedGeneration; got != tc.want {
				t.Errorf("Ready.ObservedGeneration = %d, want %d", got, tc.want)
			}
		})
	}
}

// The types that a State can hold are the ones the status writer may write, and they stay under the 16 that a Binding's
// status takes.
func TestTypesAreWhatAStateCanHold(t *testing.T) {
	got := Types()
	want := []ConditionType{BackendReachable, ChannelJoined, ApproversReadable, Ready}
	if !slices.Equal(got, want) {
		t.Errorf("Types() = %v, want %v", got, want)
	}
	got[0] = "Mutated"
	if Types()[0] != BackendReachable {
		t.Error("Types() returned the package's own slice: a caller changed it")
	}
	s := NewState()
	var all []Condition
	for _, ty := range Types() {
		all = append(all, Condition{Type: ty, Status: True, Reason: ReasonOK})
	}
	s.Set("b", t0, all...)
	for _, c := range s.Snapshot()["b"] {
		if !slices.Contains(Types(), c.Type) {
			t.Errorf("State holds %s, which Types() does not list", c.Type)
		}
	}
}

// A result is stamped with the generation the Target had when the pass began, so that an edit during the pass is not claimed as
// observed, and the next pass stamps the new one, even when nothing else about the result changed.
func TestAProbeIsStampedWithTheGenerationOfItsPass(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", Generation: 1})
	var gate chan struct{}
	var calls atomic.Int32
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		if calls.Add(1) == 1 {
			<-gate
		}
		return ok(), nil
	}}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: &fakeChannels{}, Timeout: time.Hour}
	}, func(h *harness) {
		c := h.conds("b")
		if got := c[BackendReachable].ObservedGeneration; got != 1 {
			t.Errorf("a Pending condition has ObservedGeneration %d, want the Target's 1", got)
		}
		g.set("r1", Target{Binding: "b", Channel: "C1", Generation: 2}) // the edit lands while the first probe is in flight
		close(gate)
		synctest.Wait()
		c = h.conds("b")
		for _, ty := range []ConditionType{BackendReachable, ChannelJoined, Ready} {
			if got := c[ty].ObservedGeneration; got != 1 {
				t.Errorf("%s.ObservedGeneration = %d after a pass that began at generation 1, want 1", ty, got)
			}
		}
		g.set("r2", Target{Binding: "b", Channel: "C1", Generation: 2})
		h.advance(tick)
		c = h.conds("b")
		for _, ty := range []ConditionType{BackendReachable, ChannelJoined, Ready} {
			if got := c[ty].ObservedGeneration; got != 2 {
				t.Errorf("%s.ObservedGeneration = %d after a pass at generation 2, want 2 although nothing transitioned", ty, got)
			}
		}
	})
}

// OnPass is called once for each pass, after the last probe of it has been recorded, and never while one is running, which is
// the moment a consumer can read a result that carries the generation the pass began at even though no condition changed.
func TestOnPassIsCalledOnceAfterEachPass(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
	var passes atomic.Int32
	var inProbe atomic.Int32
	var overlapped atomic.Bool
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		inProbe.Add(1)
		defer inProbe.Add(-1)
		return ok(), nil
	}}
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: &fakeChannels{},
		OnPass: func() {
			passes.Add(1)
			if inProbe.Load() != 0 {
				overlapped.Store(true)
			}
		},
	}, func(h *harness) {
		if got := passes.Load(); got != 1 {
			t.Fatalf("OnPass calls after the first pass = %d, want 1", got)
		}
		if agents.total() != 2 {
			t.Fatalf("probes = %d, want 2: OnPass came before the probes ended", agents.total())
		}
		g.set("r2", Target{Binding: "a", Channel: "C1"}, Target{Binding: "b", Channel: "C2"})
		h.nextPass(agents.total)
		if got := passes.Load(); got != 2 {
			t.Errorf("OnPass calls after the second pass = %d, want 2", got)
		}
		if overlapped.Load() {
			t.Error("OnPass was called while a probe was in flight")
		}
	})
}

// A hook that panics is reported and cannot stop the Prober: the next pass runs.
func TestAnOnPassPanicIsReportedAndDoesNotStopTheProber(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1"})
	var errs atomic.Int32
	agents := &fakeAgents{}
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: agents,
		OnPass:       func() { panic("boom") },
		OnProbeError: func(ProbeError) { errs.Add(1) },
	}, func(h *harness) {
		h.nextPass(agents.total)
		if errs.Load() < 2 {
			t.Errorf("reported %d failures after two passes, want one for each", errs.Load())
		}
	})
}
