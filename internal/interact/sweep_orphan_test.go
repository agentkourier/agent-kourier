package interact_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// orphanGrace is the default OrphanGrace: how long a due interaction must be seen without its Binding, or with its connection not served,
// before the sweep ends it without the agent.
const orphanGrace = 5 * time.Minute

// abandons is the session as the sweep ends a pause that the agent cannot be asked about. A call of any task but current is ErrNotPaused, as the
// real one answers; err fails the next calls, one each.
type abandons struct {
	mu      sync.Mutex
	current a2a.TaskID
	calls   []a2a.TaskID
	reasons []string
	errs    []error
	log     *callLog
}

func (a *abandons) Abandon(_ context.Context, _ store.ConversationKey, id a2a.TaskID, reason string) error {
	a.log.add("abandon")
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls, a.reasons = append(a.calls, id), append(a.reasons, reason)
	switch {
	case len(a.errs) > 0:
		err := a.errs[0]
		a.errs = a.errs[1:]
		return err
	case id != a.current:
		return session.ErrNotPaused
	}
	a.current = ""
	return nil
}

func (a *abandons) got() []a2a.TaskID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// orphanRig is a sweep rig whose pause is waiting on task-1 for the session, due since a minute ago.
func orphanRig(t *testing.T) *sweepRig {
	t.Helper()
	r := newSweepRig(t)
	r.abandon.current = "task-1"
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	return r
}

func (r *sweepRig) advance(d time.Duration) { *r.clock = r.clock.Add(d) }

func (r *sweepRig) sweepOnce(t *testing.T, sw *interact.Sweeper) {
	t.Helper()
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep = %v", err)
	}
}

// requireNothingDone fails if the sweep has called the agent's session or the platform, or queued a notice.
func (r *sweepRig) requireNothingDone(t *testing.T) {
	t.Helper()
	if got := r.calls.list(); len(got) != 0 {
		t.Errorf("calls %q; want none", got)
	}
	if got := r.notices(t); len(got) != 0 {
		t.Errorf("%d notices queued; want none", len(got))
	}
}

// requireOrphaned fails unless the interaction ix-1 was ended without the agent and without the thread: no cancel, no denial, no edit, no
// notice; the pause was released once; the decision audited and counted as the orphan's own.
func (r *sweepRig) requireOrphaned(t *testing.T, binding string) {
	t.Helper()
	if got := r.state(t, "ix-1"); got != store.InteractionTimedOut {
		t.Errorf("the interaction is %s; want it ended (timed out: its time ran out unanswered)", got)
	}
	if got := r.abandon.got(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
		t.Errorf("the pause was released %v; want task-1 once", got)
	}
	if got := r.sess.cancelled(); len(got) != 0 || len(r.sess.sent()) != 0 {
		t.Errorf("the session was asked to cancel %v and sent %d answers; want neither: there is no Binding to reach the agent through", got, len(r.sess.sent()))
	}
	if got := r.upd.list(); len(got) != 0 {
		t.Errorf("updates %q; want none: no platform can edit the prompt", got)
	}
	if got := r.notices(t); len(got) != 0 {
		t.Errorf("%d notices; want none", len(got))
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionOrphaned || r.aud.got[0].InteractionID != "ix-1" || r.aud.got[0].TaskID != "task-1" || r.aud.act[0].Binding != binding || r.aud.act[0].User != "" {
		t.Errorf("audit %+v by %+v; want one orphaned decision by no person, under %q", r.aud.got, r.aud.act, binding)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{binding + " orphaned"}) {
		t.Errorf("counted %q; want %q", got, binding+" orphaned")
	}
	if got := r.hooked(); len(got) != 0 {
		t.Errorf("the timeout hook was called for %q; want it left to the timeouts that were carried out", got)
	}
}

// TestSweepEndsAnInteractionWhoseBindingIsGoneOnceItHasBeenMissingForTheGrace: Session.Cancel needs the Binding's backend, so for a
// Binding that left the config it fails at every sweep for ever. The sweep does not retry that: once the Binding has been missing for the
// grace (long enough for a reload, or a takeover, to have put it back), the pause is released without the agent and the interaction ends.
// Nothing is edited or posted, the agent is not called, and the end is counted and audited as its own outcome.
func TestSweepEndsAnInteractionWhoseBindingIsGoneOnceItHasBeenMissingForTheGrace(t *testing.T) {
	for name, tc := range map[string]struct {
		mod func(*interact.SweepConfig)
	}{
		"the Binding was deleted, and its connection runs": {nil},
		"the connection was removed with its Bindings": {func(c *interact.SweepConfig) {
			c.Updaters = chat.PlatformSet[interact.Updater]{} // nothing runs it: it is down for good, and its Bindings left with it
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := orphanRig(t)
			r.look.bindings = nil
			sw := r.sweeper(t, tc.mod)

			r.sweepOnce(t, sw)
			r.requireNothingDone(t)
			if got := r.state(t, "ix-1"); got != store.InteractionPending {
				t.Fatalf("the interaction is %s at the first sight of the missing Binding; want it pending: a reload may put the Binding back", got)
			}
			r.advance(orphanGrace - time.Second)
			r.sweepOnce(t, sw)
			if got := r.state(t, "ix-1"); got != store.InteractionPending {
				t.Fatalf("the interaction is %s just inside the grace; want it pending", got)
			}
			r.requireNothingDone(t)

			r.advance(time.Second)
			r.sweepOnce(t, sw)
			r.requireOrphaned(t, "ns/b")
			if got := r.calls.list(); !slices.Equal(got, []string{"abandon", "resolve"}) {
				t.Errorf("calls %q; want the pause released, then the end recorded", got)
			}
			if got := strings.Count(r.logs.String(), "level=WARN"); got != 1 || !strings.Contains(r.logs.String(), "interaction=ix-1") {
				t.Errorf("log:\n%s\nwant one warning that names the interaction", r.logs.String())
			}

			r.advance(time.Hour)
			r.sweepOnce(t, sw) // and nothing is done twice
			if len(r.abandon.got()) != 1 || len(r.aud.got) != 1 || len(r.outcomes()) != 1 {
				t.Errorf("a later sweep ended it again: %d releases, %d audits, %d counts", len(r.abandon.got()), len(r.aud.got), len(r.outcomes()))
			}
		})
	}
}

// TestSweepDoesNotEndAnInteractionWhoseBindingIsOnlyMissingNow: the grace runs from the first sweep that found the Binding missing, and a
// sweep that finds it there starts the count again. An interaction whose Binding is back is timed out as any is, with the agent.
func TestSweepDoesNotEndAnInteractionWhoseBindingIsOnlyMissingNow(t *testing.T) {
	t.Run("back inside the grace", func(t *testing.T) {
		r := orphanRig(t)
		bindings := r.look.bindings
		r.look.bindings = nil
		sw := r.sweeper(t, nil)
		r.sweepOnce(t, sw)
		r.advance(orphanGrace - time.Minute)
		r.look.bindings = bindings // the reload mends the config
		r.advance(2 * time.Minute)
		r.sess.current = "task-1"
		r.sweepOnce(t, sw)
		if got := r.state(t, "ix-1"); got != store.InteractionTimedOut || len(r.abandon.got()) != 0 || len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionTimedOut {
			t.Errorf("state %s, %d releases, audit %+v; want it timed out as any interaction is: cancelled at the agent, by the Binding that is back", got, len(r.abandon.got()), r.aud.got)
		}
		if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
			t.Errorf("cancelled %v; want the agent asked to cancel the task", got)
		}
	})

	t.Run("missing again, the count starts over", func(t *testing.T) {
		r := orphanRig(t)
		bindings := r.look.bindings
		r.press.waiting["ix-1"] = true // a press keeps the interaction pending while the Binding is back
		sw := r.sweeper(t, nil)
		r.look.bindings = nil
		r.sweepOnce(t, sw)
		r.advance(orphanGrace - time.Minute)
		r.look.bindings = bindings
		r.sweepOnce(t, sw) // the Binding is there: the count is dropped
		r.look.bindings = nil
		r.advance(2 * time.Minute) // more than the grace since the first sweep, less than it since the Binding was last seen
		r.sweepOnce(t, sw)
		if got := r.state(t, "ix-1"); got != store.InteractionPending || len(r.abandon.got()) != 0 {
			t.Errorf("state %s, %d releases; want it pending: the Binding was not missing for the grace in one stretch", got, len(r.abandon.got()))
		}
		r.advance(orphanGrace)
		r.press.waiting["ix-1"] = false
		r.sweepOnce(t, sw)
		if got := r.state(t, "ix-1"); got != store.InteractionTimedOut || len(r.abandon.got()) != 1 {
			t.Errorf("state %s, %d releases; want it ended once the Binding has been missing for the grace", got, len(r.abandon.got()))
		}
	})
}

// TestSweepDoesNotTakeAFailedLookupForAMissingBinding: only an error that says the Binding is unknown makes an orphan. A lookup that fails for
// another reason (the config has not loaded, say) says nothing of the Binding, so however long it lasts the interaction goes down the ordinary
// timeout path and is not released without the agent.
func TestSweepDoesNotTakeAFailedLookupForAMissingBinding(t *testing.T) {
	r := orphanRig(t)
	r.sess.current = "task-1"
	r.look.err = errors.New("broker: Binding ns/b: the config has not loaded yet")
	sw := r.sweeper(t, nil)
	r.sweepOnce(t, sw)
	r.advance(2 * orphanGrace)
	r.sweepOnce(t, sw)
	if got := r.abandon.got(); len(got) != 0 {
		t.Errorf("the pause was released %v; want it left to the ordinary timeout", got)
	}
	if got := r.state(t, "ix-1"); got != store.InteractionTimedOut || !slices.Equal(r.sess.cancelled(), []a2a.TaskID{"task-1"}) {
		t.Errorf("state %s, cancelled %v; want it timed out as any is, the task cancelled through the session", got, r.sess.cancelled())
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionTimedOut || len(r.outcomes()) != 0 || len(r.hooked()) != 1 {
		t.Errorf("audit %+v, counted %q, hooks %q; want a timeout, not an orphan's end", r.aud.got, r.outcomes(), r.hooked())
	}
}

// TestSweepStillWaitsForAConnectionThatIsDownForNow: a connection that is served and does not run (it has not started yet, or its
// credentials were rejected) keeps its Bindings, so it is not an orphan's: nothing is ended however long it takes, and the interaction
// is timed out by the first sweep after the connection runs.
func TestSweepStillWaitsForAConnectionThatIsDownForNow(t *testing.T) {
	r := orphanRig(t)
	down := func(c *interact.SweepConfig) {
		c.Updaters = chat.PlatformSet[interact.Updater]{}                   // nothing runs it
		c.Served = func(conn string) bool { return conn == key.Connection } // but this broker serves it
	}
	sw := r.sweeper(t, down)
	for range 5 {
		r.sweepOnce(t, sw)
		r.advance(time.Hour)
	}
	if got := r.state(t, "ix-1"); got != store.InteractionPending {
		t.Errorf("the interaction is %s; want it pending for as long as its connection is down", got)
	}
	r.requireNothingDone(t)
	if got := r.abandon.got(); len(got) != 0 {
		t.Errorf("the pause was released %v; want it left parked: it will be answered, or timed out, when the connection runs", got)
	}

	sw = r.sweeper(t, nil) // the connection runs
	r.sess.current = "task-1"
	r.sweepOnce(t, sw)
	if got := r.state(t, "ix-1"); got != store.InteractionTimedOut || len(r.sess.cancelled()) != 1 || len(r.abandon.got()) != 0 {
		t.Errorf("state %s after the connection runs; want it timed out as any is, with the agent", got)
	}
}

// TestSweepEndsTheInteractionOfAConnectionThatIsNotServed: the Binding is in the config (a second connection of a Slack app keeps its
// Bindings in the snapshot, and says so in their status) and this broker does not serve the connection, so nothing posts for it and
// nothing will answer for it. The agent can still be reached through the Binding, so the task is cancelled there, once, and the pause is
// ended; if it cannot be, the pause is released locally.
func TestSweepEndsTheInteractionOfAConnectionThatIsNotServed(t *testing.T) {
	notServed := func(c *interact.SweepConfig) {
		c.Updaters = chat.PlatformSet[interact.Updater]{}
		c.Served = func(string) bool { return false }
	}
	run := func(t *testing.T, prepare func(*sweepRig)) *sweepRig {
		t.Helper()
		r := orphanRig(t)
		r.sess.current = "task-1"
		prepare(r)
		sw := r.sweeper(t, notServed)
		r.sweepOnce(t, sw)
		if got := r.state(t, "ix-1"); got != store.InteractionPending {
			t.Fatalf("the interaction is %s at the first sight; want it pending until the grace has passed: the other connection of the app may go", got)
		}
		r.requireNothingDone(t)
		r.advance(orphanGrace)
		r.sweepOnce(t, sw)
		return r
	}
	t.Run("the agent takes the cancel", func(t *testing.T) {
		r := run(t, func(*sweepRig) {})
		if got := r.state(t, "ix-1"); got != store.InteractionTimedOut {
			t.Errorf("the interaction is %s; want it ended", got)
		}
		if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || len(r.abandon.got()) != 0 {
			t.Errorf("cancelled %v, released locally %v; want the task cancelled at the agent, which ends the pause", got, r.abandon.got())
		}
		if got := r.upd.list(); len(got) != 0 || len(r.notices(t)) != 0 || len(r.sess.sent()) != 0 {
			t.Errorf("updates %q, %d notices, %d denials; want none: no platform reaches the thread", got, len(r.notices(t)), len(r.sess.sent()))
		}
		if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionOrphaned || !slices.Equal(r.outcomes(), []string{"ns/b orphaned"}) || len(r.hooked()) != 0 {
			t.Errorf("audit %+v, counted %q, hooks %q; want one orphaned decision, counted as such", r.aud.got, r.outcomes(), r.hooked())
		}
	})
	t.Run("the agent cannot be reached", func(t *testing.T) {
		r := run(t, func(r *sweepRig) { r.sess.always = errBoom })
		if got := r.state(t, "ix-1"); got != store.InteractionTimedOut {
			t.Errorf("the interaction is %s; want it ended though the cancel failed: it is tried once, not for ever", got)
		}
		if got := r.abandon.got(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
			t.Errorf("the pause was released %v; want it released locally", got)
		}
		if !strings.Contains(r.logs.String(), "the agent could not be asked to cancel") {
			t.Errorf("log:\n%s\nwant a line that says the task is left at the agent", r.logs.String())
		}
	})
	t.Run("the task is not paused any more", func(t *testing.T) {
		r := run(t, func(r *sweepRig) { r.sess.current = "task-9" })
		if got := r.state(t, "ix-1"); got != store.InteractionTimedOut || len(r.abandon.got()) != 0 {
			t.Errorf("the interaction is %s after %d releases; want it ended, with nothing to release", got, len(r.abandon.got()))
		}
	})
}

// TestSweepEndsAnApprovalOfABindingThatIsGoneWithoutDenyingIt: an approval cannot be denied without the Binding's dialect. It is ended like a
// question, and counted as the orphan's outcome, not as an approval that timed out.
func TestSweepEndsAnApprovalOfABindingThatIsGoneWithoutDenyingIt(t *testing.T) {
	r := newSweepRig(t)
	r.abandon.current = "task-1"
	r.approvalRowIn(t, key, "ix-1", "task-1", store.InteractionToolApproval, approvalOfTask(t, "task-1"), t0.Add(timeout))
	r.look.bindings = nil
	sw := r.sweeper(t, nil)
	r.sweepOnce(t, sw)
	r.advance(orphanGrace)
	r.sweepOnce(t, sw)
	r.requireOrphaned(t, "ns/b")
}

// TestSweepLeavesAnOrphanWithAPressWaitingUntilThePressIsDecided: a press recorded by the deadline is not dropped by the end of a pause
// that nobody can answer, any more than by a timeout; the worker decides it (a Binding that is gone: refused), and the end follows.
func TestSweepLeavesAnOrphanWithAPressWaitingUntilThePressIsDecided(t *testing.T) {
	r := orphanRig(t)
	r.look.bindings = nil
	r.press.waiting["ix-1"] = true
	sw := r.sweeper(t, nil)
	r.sweepOnce(t, sw)
	r.advance(orphanGrace)
	r.sweepOnce(t, sw)
	if got := r.state(t, "ix-1"); got != store.InteractionPending || len(r.abandon.got()) != 0 {
		t.Fatalf("state %s, %d releases; want it left while a press waits", got, len(r.abandon.got()))
	}
	r.press.waiting["ix-1"] = false
	r.sweepOnce(t, sw)
	r.requireOrphaned(t, "ns/b")
}

// TestAnEndThatFailsIsTriedAgainAndCountedOnce: releasing the pause is never given up on, as a cancel is not; a record of the end that fails
// leaves the interaction pending, and the release is asked again (ErrNotPaused then is the release done).
func TestAnEndThatFailsIsTriedAgainAndCountedOnce(t *testing.T) {
	t.Run("the release fails", func(t *testing.T) {
		r := orphanRig(t)
		r.look.bindings = nil
		r.abandon.errs = []error{errBoom}
		sw := r.sweeper(t, nil)
		r.sweepOnce(t, sw)
		r.advance(orphanGrace)
		if err := sw.Sweep(t.Context()); !errors.Is(err, errBoom) {
			t.Fatalf("Sweep = %v; want the error of the release", err)
		}
		if got := r.state(t, "ix-1"); got != store.InteractionPending || len(r.aud.got) != 0 || len(r.outcomes()) != 0 {
			t.Fatalf("state %s, %d audits, %d counts; want it pending and nothing recorded", got, len(r.aud.got), len(r.outcomes()))
		}
		r.abandon.mu.Lock()
		r.abandon.calls = nil // the failed call is not the one that released it
		r.abandon.mu.Unlock()
		r.sweepOnce(t, sw)
		r.requireOrphaned(t, "ns/b")
	})
	t.Run("the end cannot be recorded", func(t *testing.T) {
		r := orphanRig(t)
		r.look.bindings = nil
		sw := r.sweeper(t, nil)
		r.sweepOnce(t, sw)
		r.advance(orphanGrace)
		r.st.fail = true
		if err := sw.Sweep(t.Context()); !errors.Is(err, errBoom) {
			t.Fatalf("Sweep = %v; want the error of the record", err)
		}
		if got := r.state(t, "ix-1"); got != store.InteractionPending || len(r.outcomes()) != 0 {
			t.Fatalf("state %s, %d counts; want it pending and not counted", got, len(r.outcomes()))
		}
		r.sweepOnce(t, sw) // the pause is released already: the second release finds nothing paused
		if got := r.state(t, "ix-1"); got != store.InteractionTimedOut || len(r.outcomes()) != 1 || len(r.aud.got) != 1 {
			t.Errorf("state %s, %d counts, %d audits; want it ended and counted once", got, len(r.outcomes()), len(r.aud.got))
		}
	})
	t.Run("a person's answer won", func(t *testing.T) {
		r := orphanRig(t)
		r.look.bindings = nil
		sw := r.sweeper(t, nil)
		r.sweepOnce(t, sw)
		r.advance(orphanGrace)
		r.st.conflict = true
		r.sweepOnce(t, sw)
		if len(r.aud.got) != 0 || len(r.outcomes()) != 0 {
			t.Errorf("%d audits, %d counts; want none: the row's first decision stands", len(r.aud.got), len(r.outcomes()))
		}
	})
}

// TestSweepSpendsItsBatchOnTheEndsItMakes: ending a pause writes to the store and the session, so it is work, and a backlog of orphans (a
// namespace's Bindings deleted at once) is ended a batch at a time. An interaction that waits out the grace is no work.
func TestSweepSpendsItsBatchOnTheEndsItMakes(t *testing.T) {
	r := orphanRig(t)
	r.pending(t, "ix-2", "task-2", t0.Add(timeout+time.Second))
	r.look.bindings = nil
	sw := r.sweeper(t, func(c *interact.SweepConfig) { c.Batch = 1 })
	r.sweepOnce(t, sw)
	r.advance(orphanGrace)
	r.sweepOnce(t, sw)
	ended := func() int {
		n := 0
		for _, id := range []string{"ix-1", "ix-2"} {
			if r.state(t, id) == store.InteractionTimedOut {
				n++
			}
		}
		return n
	}
	if ended() != 1 {
		t.Fatalf("%d ended by a sweep with a batch of one; want one", ended())
	}
	r.sweepOnce(t, sw)
	if ended() != 2 {
		t.Errorf("%d ended after the next sweep; want both", ended())
	}
}

// TestSweepForgetsWhatItCountedOfAnInteractionThatIsSettled: the first sight of a missing Binding is remembered by interaction, and goes
// when the interaction is settled by any means, so that nothing grows for as long as the process runs.
func TestSweepForgetsWhatItCountedOfAnInteractionThatIsSettled(t *testing.T) {
	r := orphanRig(t)
	r.look.bindings = nil
	sw := r.sweeper(t, nil)
	r.sweepOnce(t, sw)
	if sw.OrphanCount() != 1 {
		t.Fatalf("%d interactions counted; want the one", sw.OrphanCount())
	}
	if err := r.st.ResolveInteraction(t.Context(), "ix-1", store.InteractionCancelled, t0); err != nil { // settled by a press, a task that ended, ...
		t.Fatal(err)
	}
	r.sweepOnce(t, sw)
	if sw.OrphanCount() != 0 {
		t.Errorf("%d interactions counted after it was settled; want none", sw.OrphanCount())
	}
}

// TestAPressOnAnInteractionOfABindingThatIsGoneIsToldSoBeforeAndAfterItEnds: the press pipeline refuses a press on an interaction whose
// Binding left the config, and tells the person so, deleting the press; once the sweep has ended the interaction it tells them the
// question timed out, deleting the press too. Both are final, so a press replayed at a takeover cannot be kept for ever.
func TestAPressOnAnInteractionOfABindingThatIsGoneIsToldSoBeforeAndAfterItEnds(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	var err error
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: &sweepLookup{}, Authorizers: chattest.Same[interact.Authorizer](r.az),
		Ephemerals: chattest.Same[interact.Ephemeral](r.eph), Decider: r.d, Auditor: r.aud, Limits: store.PressLimits{Total: 10, PerUser: 2}, Now: func() time.Time { return r.now }}))
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.process(t, r.submit(t, "UALICE1", 0))
	if err != nil || res != interact.Done || !slices.Equal(r.eph.list(), []string{"UALICE1: This question is no longer available."}) || r.waiting(t) != 0 {
		t.Fatalf("a press before the end = %v, %v, told %q, %d waiting; want it done, with the person told the question is gone", res, err, r.eph.list(), r.waiting(t))
	}
	if got := r.row(t).State; got != store.InteractionPending {
		t.Fatalf("the interaction is %s after the refused press; want it pending", got)
	}

	if err := r.st.ResolveInteraction(t.Context(), "ix-1", store.InteractionTimedOut, deadline); err != nil { // what the sweep does
		t.Fatal(err)
	}
	res, err = r.process(t, r.submit(t, "UBOB22", 0))
	if err != nil || res != interact.Done || r.waiting(t) != 0 {
		t.Fatalf("a press after the end = %v, %v, %d waiting; want it done and deleted", res, err, r.waiting(t))
	}
	if got := r.eph.list(); len(got) != 2 || got[1] != "UBOB22: This question has timed out." {
		t.Errorf("told %q; want the second person told that it timed out", got)
	}
	if len(r.sess.answers()) != 0 {
		t.Errorf("%d answers reached the session; want none", len(r.sess.answers()))
	}
}
