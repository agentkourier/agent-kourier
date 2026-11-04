package bindingcheck

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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

// Ready says whose evidence it rests on: the UID that all its conditions have, or none if they differ.
func TestReadyHasTheCommonUIDOfItsConditionsOrNone(t *testing.T) {
	s := NewState()
	s.Set("same", t0,
		Condition{Type: BackendReachable, Status: True, Reason: ReasonOK, UID: "u"}, Condition{Type: ChannelJoined, Status: True, Reason: ReasonOK, UID: "u"})
	s.Set("mixed", t0,
		Condition{Type: BackendReachable, Status: True, Reason: ReasonOK, UID: "u"}, Condition{Type: ChannelJoined, Status: True, Reason: ReasonOK, UID: "v"})
	if got := find(t, s.Snapshot()["same"], Ready).UID; got != "u" {
		t.Errorf("Ready.UID = %q, want u", got)
	}
	if got := find(t, s.Snapshot()["mixed"], Ready).UID; got != "" {
		t.Errorf("Ready.UID = %q for conditions of two objects, want none", got)
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

// A Binding that is deleted and made again has the name it had and a new UID, and restarts its generation at 1: what was found of
// the first object is not a finding about the second. Its conditions are Pending at its generation until a probe of it ends, and
// a probe that was still in flight for the first object records nothing, whatever it found.
func TestARecreatedBindingIsPendingAtItsGenerationAndAnOldProbeIsDropped(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", Generation: 7, UID: "old"})
	var gate chan struct{}
	var calls atomic.Int32
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		if calls.Add(1) == 2 {
			<-gate
		}
		return ok(), nil
	}}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: &fakeChannels{}, Timeout: time.Hour}
	}, func(h *harness) {
		c := h.conds("b")
		if c[Ready].Status != True || c[Ready].ObservedGeneration != 7 {
			t.Fatalf("Ready = %s at %d, want True at 7", c[Ready].Status, c[Ready].ObservedGeneration)
		}
		g.set("r1", Target{Binding: "b", Channel: "C1", Generation: 1, UID: "new"}) // made again: the second pass is held in its probe
		h.advance(tick)
		c = h.conds("b")
		if b := c[BackendReachable]; b.Status != Unknown || b.Reason != ReasonPending || b.ObservedGeneration != 1 || b.UID != "new" {
			t.Errorf("BackendReachable = %s/%s at %d for %q, want Unknown/Pending at the new object's generation 1 and UID", b.Status, b.Reason, b.ObservedGeneration, b.UID)
		}
		if c[Ready].Status == True || c[Ready].ObservedGeneration != 1 {
			t.Errorf("Ready = %s at %d, want it not True and at 1: the old object's True is not the new one's", c[Ready].Status, c[Ready].ObservedGeneration)
		}
		close(gate)
		synctest.Wait()
		c = h.conds("b")
		if c[Ready].Status != True || c[Ready].ObservedGeneration != 1 || c[Ready].UID != "new" {
			t.Errorf("Ready = %s at %d for %q after the new object's probe, want True at 1 for \"new\"", c[Ready].Status, c[Ready].ObservedGeneration, c[Ready].UID)
		}
	})
}

// The cause of a probe, which the status must not carry, reaches the condition for the log and is not in its JSON.
func TestACausePassesToTheConditionForTheLogAndNotIntoItsJSON(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1"})
	agents := &fakeAgents{fn: func(context.Context, string) (Outcome, error) {
		return Outcome{Status: False, Reason: ReasonUnreachable, Message: "the request failed", Cause: "dial tcp 10.0.0.5:8080: refused"}, nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents}, func(h *harness) {
		c := h.conds("b")[BackendReachable]
		if c.Message != "the request failed" || c.Cause != "dial tcp 10.0.0.5:8080: refused" {
			t.Errorf("condition = %q / %q, want the message and the cause apart", c.Message, c.Cause)
		}
		b, err := json.Marshal(c)
		if err != nil || strings.Contains(string(b), "10.0.0.5") {
			t.Errorf("JSON = %s (%v), want no cause in it", b, err)
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
