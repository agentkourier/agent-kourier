package interact_test

import (
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// These tests run the real session manager under the sweeper, with a runner that is really parked on the task: the sweep's fakes cannot show that
// the runner lets go, or that the agent is not called.

// TestTheBindingOfAParkedPauseIsDeletedAndTheSweepReleasesTheRunner is the problem of an interaction that never ends. The Binding is deleted while
// the approval waits; Session.Cancel fails for as long as it is missing, and used to be retried at every sweep for ever, with the runner parked, the
// task recorded, and the interaction pending. Now the pause is released once the Binding has been missing for the grace: the runner lets go (so
// retention may deal with the session), the task is cleared, the interaction is ended, and the agent is not called and nothing is posted.
func TestTheBindingOfAParkedPauseIsDeletedAndTheSweepReleasesTheRunner(t *testing.T) {
	ag := newScriptedAgent()
	p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	row := p.submit()
	if !p.mgr.HoldsTask(key, "task-1") {
		t.Fatal("the runner does not hold the paused task")
	}

	p.binds.gone.Store(true)
	p.clk.set(pastDeadline())
	p.sweeps(1) // sees the Binding missing
	if got := p.mgr.HoldsTask(key, "task-1"); !got {
		t.Fatal("the runner let go inside the grace")
	}
	p.clk.set(pastDeadline().Add(orphanGrace))
	p.sweeps(1)

	until(t, "the runner to let go of the task", func() bool { return !p.mgr.HoldsTask(key, "task-1") })
	until(t, "the task to be cleared from the session", func() bool {
		sess, err := p.st.GetSession(t.Context(), key, 100*365*24*time.Hour, p.clk.now())
		return err == nil && sess.TaskID == ""
	})
	if now, err := p.st.GetInteraction(t.Context(), row.ID); err != nil || now.State != store.InteractionTimedOut {
		t.Errorf("row %+v, %v; want it ended", now, err)
	}
	if ag.answered() != 0 || len(ag.cancelled()) != 0 || len(p.calls.seen()) != 0 {
		t.Errorf("the agent was sent %d answers, asked to cancel %v, and %q was audited; want it not called at all", ag.answered(), ag.cancelled(), p.calls.seen())
	}
	if got := p.notices(); len(got) != 0 {
		t.Errorf("notices %q; want none", got)
	}
	if len(p.aud.got) != 1 || p.aud.got[0].Result != audit.DecisionOrphaned || !slices.Equal(p.outcomes(), []string{"ns/b orphaned"}) {
		t.Errorf("audit %+v, counted %q; want one orphaned decision, counted as such", p.aud.got, p.outcomes())
	}
	p.sweeps(3)
	if len(p.aud.got) != 1 || len(p.outcomes()) != 1 {
		t.Errorf("later sweeps ended it again: %d audits, %d counts", len(p.aud.got), len(p.outcomes()))
	}
}

// TestAfterARestartTheSweepEndsTheInteractionOfABindingThatIsGone: the new process has no runner for the pause, only the record, and the Binding
// is gone. The sweep ends the interaction and clears the task as well.
func TestAfterARestartTheSweepEndsTheInteractionOfABindingThatIsGone(t *testing.T) {
	st, ag, clk := openStore(t), newScriptedAgent(), atTimes(&clockAt{}, t0.Add(time.Minute))
	first := start(t, st, ag, clk, true)
	row := first.submit()
	first.stop()

	second := start(t, st, ag, clk, true)
	second.binds.gone.Store(true)
	clk.set(pastDeadline())
	second.sweeps(1)
	clk.set(pastDeadline().Add(orphanGrace))
	second.sweeps(1)

	if now, err := st.GetInteraction(t.Context(), row.ID); err != nil || now.State != store.InteractionTimedOut {
		t.Fatalf("row %+v, %v; want it ended", now, err)
	}
	until(t, "the task to be cleared from the session", func() bool {
		sess, err := st.GetSession(t.Context(), key, 100*365*24*time.Hour, clk.now())
		return err == nil && sess.TaskID == ""
	})
	if ag.answered() != 0 || len(ag.cancelled()) != 0 {
		t.Errorf("the agent was sent %d answers and asked to cancel %v; want it not called", ag.answered(), ag.cancelled())
	}
}
