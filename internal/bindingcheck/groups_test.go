package bindingcheck

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeGroups is a GroupChecker that records the groups of each call and answers through fn.
type fakeGroups struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(ctx context.Context, groups []string) (Outcome, error)
}

func (f *fakeGroups) CheckGroups(ctx context.Context, groups []string) (Outcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(groups))
	f.mu.Unlock()
	if f.fn == nil {
		return ok(), nil
	}
	return f.fn(ctx, groups)
}

// count is how many probes were made of exactly these groups.
func (f *fakeGroups) count(groups ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if slices.Equal(c, groups) {
			n++
		}
	}
	return n
}

func (f *fakeGroups) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// eventLog is a log of hook calls, safe for the Prober's goroutines.
type eventLog struct {
	mu sync.Mutex
	l  []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.l = append(e.l, s)
}

func (e *eventLog) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.l)
}

func TestRemoveDropsTheConditionAndRecomputesReady(t *testing.T) {
	s := NewState()
	s.Set("b", t0,
		cond(BackendReachable, True, ReasonOK), cond(ChannelJoined, True, ReasonOK), cond(ApproversReadable, False, ReasonGroupNotFound))
	if r := find(t, s.Snapshot()["b"], Ready); r.Status != False || r.Reason != "ApproversReadable:GroupNotFound" {
		t.Fatalf("Ready before = %s/%s, want False/ApproversReadable:GroupNotFound", r.Status, r.Reason)
	}

	at := t0.Add(time.Minute)
	removed, trs := s.Remove("b", at, ApproversReadable)
	if !removed {
		t.Fatal("Remove reported that the condition was not there")
	}
	conds := s.Snapshot()["b"]
	for _, c := range conds {
		if c.Type == ApproversReadable {
			t.Errorf("ApproversReadable is still in the State: %+v", c)
		}
	}
	r := find(t, conds, Ready)
	if r.Status != True || r.Reason != ReasonOK || !r.LastTransitionTime.Equal(at) {
		t.Errorf("Ready after = %s/%s transitioned at %v, want True/OK at %v", r.Status, r.Reason, r.LastTransitionTime, at)
	}
	if len(trs) != 1 || trs[0].Binding != "b" || trs[0].Old.Status != False || trs[0].New.Type != Ready || trs[0].New.Status != True {
		t.Errorf("transitions = %+v, want one: Ready from False to True", trs)
	}
}

func TestRemoveOfAConditionThatIsNotThereOrNotProbedChangesNothing(t *testing.T) {
	s := NewState()
	s.Set("b", t0, cond(BackendReachable, True, ReasonOK))
	before := s.Snapshot()
	for _, tc := range []struct {
		name    string
		binding string
		typ     ConditionType
	}{
		{"a probed type the Binding lacks", "b", ApproversReadable},
		{"Ready, which is derived", "b", Ready},
		{"a type that is not one", "b", "Bogus"},
		{"a Binding the State lacks", "other", ApproversReadable},
	} {
		removed, trs := s.Remove(tc.binding, t0.Add(time.Minute), tc.typ)
		if removed || len(trs) != 0 {
			t.Errorf("%s: Remove = %v, %+v; want false and no transition", tc.name, removed, trs)
		}
	}
	if got := s.Snapshot(); len(got["b"]) != len(before["b"]) || len(got) != 1 {
		t.Errorf("the State changed: %+v, was %+v", got, before)
	}
}

// A Binding whose last probed condition goes has nothing left to derive Ready from, and is not left holding a Ready of its own.
func TestRemoveOfTheLastConditionLeavesNoBinding(t *testing.T) {
	s := NewState()
	s.Set("b", t0, cond(ApproversReadable, True, ReasonOK))
	if removed, trs := s.Remove("b", t0, ApproversReadable); !removed || len(trs) != 0 {
		t.Errorf("Remove = %v, %+v; want true and no transition", removed, trs)
	}
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot = %+v, want empty", snap)
	}
}

func TestAGroupsProbeIsMadeOncePerTargetThatHasGroupsAndCountsTowardReady(t *testing.T) {
	g := &targets{}
	g.set("r1",
		Target{Binding: "with", Channel: "C1", ApproverGroups: []string{"S1", "S2"}},
		Target{Binding: "without", Channel: "C2"})
	groups := &fakeGroups{fn: func(context.Context, []string) (Outcome, error) {
		return Outcome{Status: False, Reason: ReasonGroupNotFound, Message: "approverGroups[1] does not exist"}, nil
	}}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups}, func(h *harness) {
		if groups.total() != 1 || groups.count("S1", "S2") != 1 {
			t.Fatalf("group probes = %v, want one, of both groups of the one Binding that has them", groups.calls)
		}
		c := h.conds("with")
		wantCond(t, c, ApproversReadable, False, ReasonGroupNotFound)
		wantCond(t, c, Ready, False, "ApproversReadable:GroupNotFound")
		if c[ApproversReadable].Message != "approverGroups[1] does not exist" {
			t.Errorf("Message = %q", c[ApproversReadable].Message)
		}
		wantCond(t, h.conds("without"), Ready, True, ReasonOK)
		if _, found := h.conds("without")[ApproversReadable]; found {
			t.Error("a Binding with no groups has an ApproversReadable condition")
		}

		h.nextPass(groups.total)
		if groups.total() != 2 {
			t.Errorf("group probes after the second pass = %d, want 2", groups.total())
		}
	})
}

func TestGroupsAreNotTrackedWithoutAGroupChecker(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", ApproverGroups: []string{"S1"}})
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}}, func(h *harness) {
		c := h.conds("b")
		if _, found := c[ApproversReadable]; found {
			t.Error("ApproversReadable is set without a GroupChecker")
		}
		wantCond(t, c, Ready, True, ReasonOK)
	})
}

func TestAGroupsProbeIsPendingUntilItEnds(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", ApproverGroups: []string{"S1"}})
	var gate chan struct{}
	groups := &fakeGroups{fn: func(context.Context, []string) (Outcome, error) { <-gate; return ok(), nil }}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups, Timeout: time.Hour}
	}, func(h *harness) {
		c := h.conds("b")
		wantCond(t, c, ApproversReadable, Unknown, ReasonPending)
		wantCond(t, c, Ready, Unknown, "ApproversReadable:Pending")
		if !c[ApproversReadable].LastProbeTime.IsZero() {
			t.Errorf("a Pending condition has LastProbeTime %v", c[ApproversReadable].LastProbeTime)
		}
		close(gate)
		synctest.Wait()
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)
	})
}

// Group probes take turns with the agent and channel probes, so that backends that hang hold neither back.
func TestGroupProbesAlternateWithTheOthersSoHangingBackendsDoNotHoldThemBack(t *testing.T) {
	g := &targets{}
	var ts []Target
	for _, b := range []string{"b0", "b1", "b2", "b3"} {
		ts = append(ts, Target{Binding: b, Channel: "C" + b, ApproverGroups: []string{"S" + b}})
	}
	g.set("r1", ts...)
	agents := &fakeAgents{fn: func(ctx context.Context, _ string) (Outcome, error) { <-ctx.Done(); return Outcome{}, ctx.Err() }}
	chans := &fakeChannels{fn: func(ctx context.Context, _ string) (Outcome, error) { <-ctx.Done(); return Outcome{}, ctx.Err() }}
	groups := &fakeGroups{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: chans, Groups: groups, Concurrency: 3}, func(_ *harness) {
		// Three slots, all taken by the first backend, channel and group probes. Were the group jobs last, none would run
		// before the hanging ones time out.
		if groups.total() == 0 {
			t.Error("no group was probed while the first backends and channels hung")
		}
	})
}

func TestABindingThatGainsGroupsIsPendingThenProbedAndReadyFollows(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", Generation: 1})
	var gate chan struct{}
	var ev eventLog
	groups := &fakeGroups{fn: func(context.Context, []string) (Outcome, error) {
		<-gate
		return Outcome{Status: False, Reason: ReasonMissingScope}, nil
	}}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{
			Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups, Timeout: time.Hour,
			OnTransition: func(tr Transition) {
				ev.add("transition " + string(tr.New.Type) + " " + string(tr.New.Status) + "/" + string(tr.New.Reason))
			},
		}
	}, func(h *harness) {
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)
		if _, found := h.conds("b")[ApproversReadable]; found {
			t.Fatal("a Binding with no groups has the condition")
		}

		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "u", Generation: 2, ApproverGroups: []string{"S1"}}) // an edit: same object
		h.advance(tick)
		c := h.conds("b")
		wantCond(t, c, ApproversReadable, Unknown, ReasonPending)
		wantCond(t, c, Ready, Unknown, "ApproversReadable:Pending")
		if c[ApproversReadable].ObservedGeneration != 2 || c[ApproversReadable].UID != "u" {
			t.Errorf("the Pending condition is for generation %d and UID %q, want 2 and u", c[ApproversReadable].ObservedGeneration, c[ApproversReadable].UID)
		}
		if got := ev.list(); !slices.Contains(got, "transition Ready Unknown/ApproversReadable:Pending") {
			t.Errorf("events = %v, want the change of Ready to Pending reported", got)
		}

		close(gate)
		synctest.Wait()
		wantCond(t, h.conds("b"), ApproversReadable, False, ReasonMissingScope)
		wantCond(t, h.conds("b"), Ready, False, "ApproversReadable:MissingScope")
	})
}

func TestABindingThatLosesItsGroupsLosesTheConditionAndReadyIsRecomputed(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", ApproverGroups: []string{"S1"}})
	var ev eventLog
	groups := &fakeGroups{fn: func(context.Context, []string) (Outcome, error) {
		return Outcome{Status: False, Reason: ReasonGroupNotFound}, nil
	}}
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups,
		OnTransition: func(tr Transition) { ev.add("transition " + string(tr.New.Type) + " " + string(tr.New.Status)) },
		OnDrop:       func(binding string, c ConditionType) { ev.add("drop " + binding + " " + string(c)) },
	}, func(h *harness) {
		wantCond(t, h.conds("b"), Ready, False, "ApproversReadable:GroupNotFound")

		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "u"}) // the edit takes the groups away
		h.advance(tick)
		c := h.conds("b")
		if _, found := c[ApproversReadable]; found {
			t.Errorf("the condition is still there: %+v", c[ApproversReadable])
		}
		wantCond(t, c, Ready, True, ReasonOK)

		got := ev.list()
		ready := slices.Index(got, "transition Ready True")
		drop := slices.Index(got, "drop b ApproversReadable")
		if ready < 0 || drop < 0 || ready > drop {
			t.Errorf("events = %v, want the change of Ready reported, then the drop", got)
		}
		if n := strings.Count(strings.Join(got, "\n"), "drop "); n != 1 {
			t.Errorf("%d drops, want 1: %v", n, got)
		}
		before := groups.total()
		h.advance(3 * interval)
		if groups.total() != before {
			t.Errorf("a Binding with no groups was probed for them: %d probes, had %d", groups.total(), before)
		}
		if n := strings.Count(strings.Join(ev.list(), "\n"), "drop "); n != 1 {
			t.Errorf("the drop was reported again: %v", ev.list())
		}
	})
}

// A Binding that is deleted and made again under the same name is another object: the conditions of the first are replaced, and
// one the second does not ask for goes.
func TestABindingMadeAgainWithoutGroupsDoesNotKeepTheOldConditionOfThem(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "old", ApproverGroups: []string{"S1"}})
	var ev eventLog
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: &fakeGroups{},
		OnDrop: func(binding string, c ConditionType) { ev.add("drop " + binding + " " + string(c)) },
	}, func(h *harness) {
		wantCond(t, h.conds("b"), ApproversReadable, True, ReasonOK)
		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "new"})
		h.advance(tick)
		c := h.conds("b")
		if _, found := c[ApproversReadable]; found {
			t.Errorf("the new object has the old one's condition: %+v", c[ApproversReadable])
		}
		wantCond(t, c, Ready, True, ReasonOK)
		if got := ev.list(); !slices.Equal(got, []string{"drop b ApproversReadable"}) {
			t.Errorf("events = %v, want the one drop", got)
		}
	})
}

// A Binding with no groups that is made again with some has them Pending, like any new object's conditions.
func TestABindingMadeAgainWithGroupsHasThemPending(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "old"})
	var gate chan struct{}
	groups := &fakeGroups{fn: func(context.Context, []string) (Outcome, error) { <-gate; return ok(), nil }}
	runIn(t, func() Options {
		gate = make(chan struct{})
		return Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups, Timeout: time.Hour}
	}, func(h *harness) {
		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "new", ApproverGroups: []string{"S1"}})
		h.advance(tick)
		wantCond(t, h.conds("b"), ApproversReadable, Unknown, ReasonPending)
		close(gate)
		synctest.Wait()
		wantCond(t, h.conds("b"), ApproversReadable, True, ReasonOK)
	})
}

// A Binding that keeps its groups but changes them is probed again by the pass its revision calls for; no condition is dropped
// or made Pending.
func TestABindingThatChangesItsGroupsIsProbedWithTheNewOnesAtTheNextPass(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", ApproverGroups: []string{"S1"}})
	groups := &fakeGroups{fn: func(_ context.Context, gs []string) (Outcome, error) {
		if slices.Contains(gs, "S2") {
			return Outcome{Status: False, Reason: ReasonGroupNotFound}, nil
		}
		return ok(), nil
	}}
	var ev eventLog
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups,
		OnDrop: func(binding string, c ConditionType) { ev.add("drop " + binding + " " + string(c)) },
	}, func(h *harness) {
		wantCond(t, h.conds("b"), ApproversReadable, True, ReasonOK)
		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "u", ApproverGroups: []string{"S1", "S2"}})
		h.advance(tick)
		if groups.count("S1", "S2") != 1 {
			t.Errorf("probes of the new groups = %d, want 1", groups.count("S1", "S2"))
		}
		wantCond(t, h.conds("b"), ApproversReadable, False, ReasonGroupNotFound)
		if got := ev.list(); len(got) != 0 {
			t.Errorf("events = %v, want none", got)
		}
	})
}

func TestAGroupsCheckerThatFailsLeavesTheConditionUnknownAndTheOthersRun(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", ApproverGroups: []string{"S1"}})
	groups := &fakeGroups{fn: func(context.Context, []string) (Outcome, error) { return Outcome{}, errors.New("boom") }}
	var errs []ProbeError
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups,
		OnProbeError: func(pe ProbeError) { errs = append(errs, pe) },
	}, func(h *harness) {
		c := h.conds("b")
		wantCond(t, c, ApproversReadable, Unknown, ReasonProbeFailed)
		wantCond(t, c, BackendReachable, True, ReasonOK)
		wantCond(t, c, ChannelJoined, True, ReasonOK)
		if len(errs) != 1 || errs[0].Condition != ApproversReadable || errs[0].Target.Binding != "b" {
			t.Errorf("probe errors = %+v, want one for ApproversReadable of b", errs)
		}
	})
}

func TestAnOnDropThatPanicsIsReportedAndStopsNothing(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", ApproverGroups: []string{"S1"}})
	var errs []ProbeError
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Groups: &fakeGroups{},
		OnDrop:       func(string, ConditionType) { panic("drop boom") },
		OnProbeError: func(pe ProbeError) { errs = append(errs, pe) },
	}, func(h *harness) {
		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "u"})
		h.advance(tick)
		var panicked *PanicError
		if len(errs) != 1 || !errors.As(errs[0].Err, &panicked) || !strings.Contains(errs[0].Err.Error(), "OnDrop") {
			t.Fatalf("probe errors = %+v, want one PanicError for OnDrop", errs)
		}
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)
	})
}

// A Binding that leaves Targets is pruned, and its Pending or probed condition goes with it without a drop.
func TestAPrunedBindingIsNotDropped(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", ApproverGroups: []string{"S1"}})
	var ev eventLog
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Groups: &fakeGroups{},
		OnDrop:  func(string, ConditionType) { ev.add("drop") },
		OnPrune: func(names []string) { ev.add("prune " + strings.Join(names, ",")) },
	}, func(h *harness) {
		g.set("r2")
		h.advance(tick)
		if got := ev.list(); !slices.Equal(got, []string{"prune b"}) {
			t.Errorf("events = %v, want the prune alone", got)
		}
	})
}

// An edit that gives a Binding its first group calls for a pass by itself, as a new Binding does: the Revision is the same here, so the
// pass that probes the group is the one the gain calls for, at the next Tick, and not the one the Interval brings a minute later.
func TestABindingThatGainsGroupsIsProbedAtTheNextTickWhateverTheRevision(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u"})
	groups := &fakeGroups{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups}, func(h *harness) {
		g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", ApproverGroups: []string{"S1"}}) // the Revision is still r1
		h.advance(tick)
		if groups.count("S1") != 1 {
			t.Errorf("group probes after one Tick = %d, want 1: the gain must call for a pass", groups.count("S1"))
		}
		wantCond(t, h.conds("b"), ApproversReadable, True, ReasonOK)
	})
}

// passGroups is a PassGroupChecker that gives each pass a fakeGroups of its own, and fails a probe made outside a pass.
type passGroups struct {
	mu     sync.Mutex
	passes []*fakeGroups
}

func (p *passGroups) CheckGroups(context.Context, []string) (Outcome, error) {
	return Outcome{}, errors.New("probed outside a pass")
}

func (p *passGroups) ForPass() GroupChecker {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := &fakeGroups{}
	p.passes = append(p.passes, f)
	return f
}

func (p *passGroups) pass(i int) *fakeGroups {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.passes) {
		return nil
	}
	return p.passes[i]
}

func (p *passGroups) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.passes) }

// A PassGroupChecker is asked for a checker of its own at the start of each pass that probes groups, and every group probe of the pass
// goes to that checker, so that it can share a read between the Targets that name one group; a pass with no group to probe asks for none.
func TestEachPassProbesGroupsWithTheCheckerOfThatPass(t *testing.T) {
	g := &targets{}
	g.set("r1",
		Target{Binding: "a", Channel: "C1", ApproverGroups: []string{"S1"}},
		Target{Binding: "b", Channel: "C2", ApproverGroups: []string{"S1", "S2"}},
		Target{Binding: "none", Channel: "C3"})
	groups := &passGroups{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups}, func(h *harness) {
		if groups.count() != 1 || groups.pass(0).count("S1") != 1 || groups.pass(0).count("S1", "S2") != 1 || groups.pass(0).total() != 2 {
			t.Fatalf("after the first pass: %d checkers; want one, that probed each Binding with groups once", groups.count())
		}
		wantCond(t, h.conds("a"), ApproversReadable, True, ReasonOK)
		wantCond(t, h.conds("b"), ApproversReadable, True, ReasonOK)

		h.nextPass(groups.count)
		if groups.count() != 2 || groups.pass(1).total() != 2 || groups.pass(0).total() != 2 {
			t.Errorf("after the second pass: %d checkers; want a second, that probed both Bindings, and the first left alone", groups.count())
		}
	})

	g = &targets{}
	g.set("r1", Target{Binding: "none", Channel: "C3"})
	groups = &passGroups{}
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups}, func(*harness) {
		if groups.count() != 0 {
			t.Errorf("a pass with no group to probe asked for %d checkers; want none", groups.count())
		}
	})
}

// panickyPass is a PassGroupChecker whose ForPass panics.
type panickyPass struct{ fakeGroups }

func (*panickyPass) ForPass() GroupChecker { panic("no pass for you") }

// A ForPass that panics is the caller's failure, reported with no condition, and the pass probes with the Groups it was given.
func TestAForPassThatPanicsIsReportedAndThePassProbesWithGroups(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "a", Channel: "C1", ApproverGroups: []string{"S1"}})
	groups := &panickyPass{}
	var mu sync.Mutex
	var errs []ProbeError
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}, Groups: groups,
		OnProbeError: func(pe ProbeError) { mu.Lock(); errs = append(errs, pe); mu.Unlock() }}, func(h *harness) {
		mu.Lock()
		defer mu.Unlock()
		var pe *PanicError
		if len(errs) != 1 || errs[0].Condition != "" || !errors.As(errs[0].Err, &pe) || !strings.Contains(errs[0].Err.Error(), "ForPass") {
			t.Errorf("OnProbeError calls = %+v; want one PanicError of ForPass with no condition", errs)
		}
		if groups.count("S1") != 1 {
			t.Errorf("probes of Groups itself = %d; want 1", groups.total())
		}
		wantCond(t, h.conds("a"), ApproversReadable, True, ReasonOK)
	})
}
