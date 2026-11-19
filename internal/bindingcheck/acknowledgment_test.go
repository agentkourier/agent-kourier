package bindingcheck

import (
	"slices"
	"strings"
	"testing"
)

// A Target whose acknowledgment names a reaction its platform cannot show has AcknowledgmentShown False with NoReactions, set by the
// Prober itself with no checker, and it holds Ready back. A Target that does not say so has no such condition.
func TestAnAcknowledgmentThatIsNotShownIsFalseAndHoldsReadyBack(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "unshown", Channel: "C1", UID: "u", Generation: 3, AcknowledgmentUnshown: true}, Target{Binding: "shown", Channel: "C2"})
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}}, func(h *harness) {
		c := h.conds("unshown")
		wantCond(t, c, AcknowledgmentShown, False, ReasonNoReactions)
		wantCond(t, c, Ready, False, "AcknowledgmentShown:NoReactions")
		if got := c[AcknowledgmentShown]; got.Message == "" || got.ObservedGeneration != 3 || got.UID != "u" || got.LastProbeTime.IsZero() {
			t.Errorf("condition = %+v; want a message, generation 3, UID u and the time of the pass", got)
		}
		if _, found := h.conds("shown")[AcknowledgmentShown]; found {
			t.Error("a Binding whose acknowledgment is shown has an AcknowledgmentShown condition")
		}
		wantCond(t, h.conds("shown"), Ready, True, ReasonOK)
	})
}

// An edit that makes the acknowledgment unshown makes the condition Pending at the next Tick, and the pass that follows sets it; an edit
// that makes it shown again removes it (OnDrop) and Ready is recomputed. Each pass stamps the condition with the Target's generation.
func TestAnAcknowledgmentThatStopsAndStartsBeingShownGainsAndLosesTheCondition(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "u", Generation: 1})
	var ev eventLog
	agents := &fakeAgents{}
	run(t, Options{
		Targets: g.Targets, Revision: g.Revision, Agents: agents, Channels: &fakeChannels{},
		OnTransition: func(tr Transition) {
			ev.add("transition " + string(tr.New.Type) + " " + string(tr.New.Status) + "/" + string(tr.New.Reason))
		},
		OnDrop: func(binding string, c ConditionType) { ev.add("drop " + binding + " " + string(c)) },
	}, func(h *harness) {
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)

		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "u", Generation: 2, AcknowledgmentUnshown: true})
		h.advance(tick)
		h.nextPass(func() int { return agents.count("b") })
		wantCond(t, h.conds("b"), AcknowledgmentShown, False, ReasonNoReactions)
		wantCond(t, h.conds("b"), Ready, False, "AcknowledgmentShown:NoReactions")
		if got := ev.list(); !slices.Contains(got, "transition AcknowledgmentShown False/NoReactions") || !slices.Contains(got, "transition Ready False/AcknowledgmentShown:NoReactions") {
			t.Errorf("events = %v, want the condition and Ready reported", got)
		}

		g.set("r3", Target{Binding: "b", Channel: "C1", UID: "u", Generation: 3, AcknowledgmentUnshown: true}) // an edit of something else
		h.nextPass(func() int { return agents.count("b") })
		if got := h.conds("b")[AcknowledgmentShown].ObservedGeneration; got != 3 {
			t.Errorf("ObservedGeneration after the pass = %d, want 3", got)
		}

		g.set("r4", Target{Binding: "b", Channel: "C1", UID: "u", Generation: 4})
		h.advance(tick)
		if _, found := h.conds("b")[AcknowledgmentShown]; found {
			t.Errorf("the condition is still there: %+v", h.conds("b")[AcknowledgmentShown])
		}
		wantCond(t, h.conds("b"), Ready, True, ReasonOK)
		if n := strings.Count(strings.Join(ev.list(), "\n"), "drop b AcknowledgmentShown"); n != 1 {
			t.Errorf("events = %v, want one drop of the condition", ev.list())
		}
	})
}

// A Binding made again under the same name is another object: one whose acknowledgment is shown does not keep the old one's condition.
func TestABindingMadeAgainWithItsAcknowledgmentShownDoesNotKeepTheCondition(t *testing.T) {
	g := &targets{}
	g.set("r1", Target{Binding: "b", Channel: "C1", UID: "old", AcknowledgmentUnshown: true})
	run(t, Options{Targets: g.Targets, Revision: g.Revision, Agents: &fakeAgents{}, Channels: &fakeChannels{}}, func(h *harness) {
		wantCond(t, h.conds("b"), AcknowledgmentShown, False, ReasonNoReactions)
		g.set("r2", Target{Binding: "b", Channel: "C1", UID: "new"})
		h.advance(tick)
		if _, found := h.conds("b")[AcknowledgmentShown]; found {
			t.Errorf("the new object has the old one's condition: %+v", h.conds("b")[AcknowledgmentShown])
		}
	})
}
