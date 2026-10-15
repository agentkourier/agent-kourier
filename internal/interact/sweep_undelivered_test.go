package interact_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A timeout's denial is sent by the session after Answer returns, and a send that fails reopens the pause: the session then
// tells Reopened, and the next sweep cancels the task and tells the thread. The tests below play the session with
// Reopened.AnswerReopened on the sweep rig; undelivered_session_test.go runs the real session manager.

const undeliveredNotice = "The agent did not take Agent Kourier's denial of this request, so Agent Kourier is cancelling the task."

// reopen plays the session telling the watcher that the pause of task-1 is open again because the denial of ix-1 was not
// delivered, and returns whether the watcher says it tells the thread.
func (r *sweepRig) reopen(t *testing.T) bool {
	t.Helper()
	return r.reopened.AnswerReopened(t.Context(), key, "task-1", "ix-1/answer")
}

// timedOutAndDenied is a rig whose approval ix-1 was swept once: the session took the denial, and the timeout is recorded.
func timedOutAndDenied(t *testing.T) (*sweepRig, *interact.Sweeper) {
	t.Helper()
	r := onlyApproval(t)
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.state(t, "ix-1") != store.InteractionTimedOut || len(r.sess.sent()) != 1 {
		t.Fatalf("state %v, %d denials; want it timed out, denied once", r.state(t, "ix-1"), len(r.sess.sent()))
	}
	return r, sw
}

// TestADenialTheAgentDidNotTakeEndsInACancelAndOneNotice: the session took the denial, so the sweep said it was denying it;
// then the pause is open again. The next sweep cancels the task, names it, and tells the thread once, with a notice of its own.
// Nothing is denied again, the timeout is not audited or counted again, and later sweeps do nothing.
func TestADenialTheAgentDidNotTakeEndsInACancelAndOneNotice(t *testing.T) {
	r, sw := timedOutAndDenied(t)
	if !r.reopen(t) {
		t.Error("the watcher left the telling to the session; want it to say it tells the thread, for the denial of an approval")
	}
	if got := r.sess.cancelled(); len(got) != 0 {
		t.Fatalf("cancelled %v before the sweep; want the watcher to do no I/O", got)
	}
	for range 4 {
		if err := sw.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
		t.Errorf("cancelled %v; want the task, once", got)
	}
	if got := len(r.sess.sent()); got != 1 {
		t.Errorf("%d denials sent; want the one: a denial that was not delivered is not sent again", got)
	}
	notices := r.noticeTexts(t)
	if len(notices) != 2 || !strings.Contains(notices[0], deniedNotice) || !strings.Contains(notices[1], undeliveredNotice) {
		t.Errorf("notices %q; want the sweep's, and then one that says the denial was not taken and the task is being cancelled", notices)
	}
	if _, err := r.st.GetInteraction(t.Context(), "ix-1"); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut {
		t.Errorf("state %v, %v; want it left timed out", r.state(t, "ix-1"), err)
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionTimedOut || !slices.Equal(r.outcomes(), []string{"ns/b timed_out"}) || len(r.hooked()) != 1 {
		t.Errorf("audit %+v, counted %q, hooked %q; want the timeout audited, counted and hooked once, and nothing for the cancel", r.aud.got, r.outcomes(), r.hooked())
	}
	if sw.UndeliveredCount() != 0 {
		t.Error("the sweeper still holds a denial it dealt with")
	}
}

// TestTheNoticeOfADenialThatWasNotTakenIsPostedOnceFromItsOwnSource: its outbox ID is the interaction's, with a suffix, so
// it is not the timeout's notice and is one post however often it is enqueued (the sweeper of another process says it too).
func TestTheNoticeOfADenialThatWasNotTakenIsPostedOnceFromItsOwnSource(t *testing.T) {
	r, sw := timedOutAndDenied(t)
	r.reopen(t)
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := r.notices(t)
	ids := []string{rows[0].ID, rows[1].ID}
	want := []string{outbox.ID("ix-1", store.OutboxNotice), outbox.ID("ix-1/undelivered", store.OutboxNotice)}
	if len(rows) != 2 || !slices.Equal(ids, want) {
		t.Fatalf("outbox rows %v; want the timeout's and the denial's, each from its own source", ids)
	}
	other := r.sweeper(t, nil) // another process: it enqueues the same notice
	r.reopen(t)
	r.sess.mu.Lock()
	r.sess.current = "task-1" // paused again, so that this sweeper's cancel is made and its notice enqueued
	r.sess.mu.Unlock()
	if err := other.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.notices(t)); got != 2 {
		t.Errorf("%d notices after another sweeper told it again; want the same two", got)
	}
}

// TestADenialTheAgentDidNotTakeWaitsForItsTimeoutToBeRecorded: the session can fail the send before the sweep records the
// timeout. The cancel and the notice wait for the record, so a notice never comes before the one it follows, and the cancel is
// not made for a row that a press could still decide.
func TestADenialTheAgentDidNotTakeWaitsForItsTimeoutToBeRecorded(t *testing.T) {
	r := onlyApproval(t)
	r.st.fail = true // the record fails once
	sw := r.sweeper(t, nil)
	r.sess.onAnswer = func() { r.reopen(t) } // the send fails as soon as it is taken
	if err := sw.Sweep(t.Context()); err == nil {
		t.Fatal("a sweep that could not record the timeout reported success")
	}
	if got := r.sess.cancelled(); len(got) != 0 {
		t.Fatalf("cancelled %v while the row was still pending; want it to wait for the record", got)
	}
	if err := sw.Sweep(t.Context()); err != nil { // records the timeout, and then deals with the denial in the same sweep
		t.Fatal(err)
	}
	notices := r.noticeTexts(t)
	if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || len(notices) != 2 || !strings.Contains(notices[0], deniedNotice) || !strings.Contains(notices[1], undeliveredNotice) {
		t.Errorf("cancelled %v, notices %q; want the timeout's notice, then the cancel and the notice of the denial", got, notices)
	}
	if len(r.sess.sent()) != 1 || len(r.aud.got) != 1 || len(r.outcomes()) != 1 {
		t.Errorf("%d denials, %d audits, counted %q; want one of each", len(r.sess.sent()), len(r.aud.got), r.outcomes())
	}
}

// TestACancelThatFailsIsTriedAgainEverySweepAndTheThreadIsToldOnce: the pause stays open behind a closed row, so only this sweep
// would end it, and it never gives up on the cancel. The thread is told at the first try, whether or not it works, and is not
// told again when it does.
func TestACancelThatFailsIsTriedAgainEverySweepAndTheThreadIsToldOnce(t *testing.T) {
	r, sw := timedOutAndDenied(t)
	r.sess.always = errBoom
	r.reopen(t)
	for i := 1; i <= 12; i++ {
		if err := sw.Sweep(t.Context()); err == nil {
			t.Fatalf("sweep %d with a failing cancel reported success", i)
		}
		if got := len(r.sess.cancelled()); got != i {
			t.Fatalf("after sweep %d the cancel was made %d times; want it tried each sweep", i, got)
		}
		if got := len(r.notices(t)); got != 2 {
			t.Fatalf("after sweep %d %d notices; want the thread told once, at the first try", i, got)
		}
	}
	r.sess.mu.Lock()
	r.sess.always = nil
	r.sess.mu.Unlock()
	for range 3 {
		if err := sw.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(r.sess.cancelled()); got != 13 || sw.UndeliveredCount() != 0 || len(r.notices(t)) != 2 {
		t.Errorf("%d cancels, %d held, %d notices; want the 13th to work and be the last, with nothing held and the thread told once", got, sw.UndeliveredCount(), len(r.notices(t)))
	}
	if len(r.sess.sent()) != 1 || len(r.aud.got) != 1 {
		t.Errorf("%d denials, %d audits; want one of each: nothing is denied or audited again", len(r.sess.sent()), len(r.aud.got))
	}
}

// TestACancelThatFindsTheTaskNotPausedSaysNothing: the thread moved on, or a person stopped the task. The cancel names the old
// task, so a newer one is left alone, and the thread is told nothing more.
func TestACancelThatFindsTheTaskNotPausedSaysNothing(t *testing.T) {
	r, sw := timedOutAndDenied(t)
	r.sess.current = "task-2" // the thread is paused on a newer task
	r.reopen(t)
	for range 3 {
		if err := sw.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || r.sess.current != "task-2" {
		t.Errorf("cancelled %v, current %q; want only task-1 asked for, and task-2 left paused", got, r.sess.current)
	}
	if got := len(r.notices(t)); got != 1 || sw.UndeliveredCount() != 0 {
		t.Errorf("%d notices, %d held; want only the sweep's notice, and nothing held", got, sw.UndeliveredCount())
	}
}

// TestAPersonsAnswerThatWasNotDeliveredIsLeftToTheSession: the interaction is answered, so the watcher keeps nothing and does
// not say it tells the thread, and no sweep cancels the task. Neither does an ask-user question, nor an answer ID of no
// interaction, or one that is not an answer's.
func TestAPersonsAnswerThatWasNotDeliveredIsLeftToTheSession(t *testing.T) {
	r := onlyApproval(t)
	r.pending(t, "ix-q", "task-1", t0.Add(timeout+time.Hour)) // a question, not due
	answer := []byte(`{"request_id":"req-task-1","decisions":[{"tool_id":"t1","approved":true},{"tool_id":"t2","approved":true}]}`)
	if err := r.st.SubmitInteraction(t.Context(), "ix-1", answer, "UALICE1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ix-1/answer", "ix-q/answer", "ix-nope/answer", "ix-1/else", "ix-1"} {
		if r.reopened.AnswerReopened(t.Context(), key, "task-1", id) {
			t.Errorf("the watcher says it tells the thread of %q; want the session to", id)
		}
	}
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.sess.cancelled(); len(got) != 0 || sw.UndeliveredCount() != 0 || len(r.notices(t)) != 0 {
		t.Errorf("cancelled %v, %d held, %d notices; want nothing done for answers that are a person's or no denial", got, sw.UndeliveredCount(), len(r.notices(t)))
	}
}

// TestADenialIsKnownByTheRowNotByTheEvent: the watcher says it tells the thread for an approval that is pending or timed out,
// and for a nested one, and not for the same approval once answered.
func TestADenialIsKnownByTheRowNotByTheEvent(t *testing.T) {
	r := newSweepRig(t)
	r.approvalRow(t, "ix-a", "task-1", store.InteractionToolApproval, approvalOfTask(t, "task-1"), t0.Add(timeout))
	r.approvalRow(t, "ix-n", "task-1", store.InteractionNested, nestedRequest(t), t0.Add(timeout))
	r.approvalRow(t, "ix-t", "task-1", store.InteractionToolApproval, approvalOfTask(t, "task-1"), t0.Add(timeout))
	if err := r.st.ResolveInteraction(t.Context(), "ix-t", store.InteractionTimedOut, t0.Add(timeout)); err != nil {
		t.Fatal(err)
	}
	r.approvalRow(t, "ix-p", "task-1", store.InteractionToolApproval, approvalOfTask(t, "task-1"), t0.Add(timeout))
	if err := r.st.SubmitInteraction(t.Context(), "ix-p", []byte(`{}`), "UALICE1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"ix-a": true, "ix-n": true, "ix-t": true, "ix-p": false} {
		if got := r.reopened.AnswerReopened(t.Context(), key, "task-1", id+"/answer"); got != want {
			t.Errorf("AnswerReopened(%s) = %v; want %v", id, got, want)
		}
	}
}

// failingGet is a store that cannot read an interaction.
type failingGet struct {
	store.Interactions
}

func (failingGet) GetInteraction(context.Context, string) (store.Interaction, error) {
	return store.Interaction{}, errBoom
}

// TestAnEventThatCannotBeJudgedIsKeptForTheSweepToJudge: the watcher's read fails, so it cannot say whose answer it was. It
// keeps the pause, says nothing is told (the session's notice stands), and the sweeper, reading the row itself, cancels the task
// of a denial and drops a person's answer.
func TestAnEventThatCannotBeJudgedIsKeptForTheSweepToJudge(t *testing.T) {
	r, _ := timedOutAndDenied(t)
	blind := interact.NewReopened(failingGet{r.st})
	if blind.AnswerReopened(t.Context(), key, "task-1", "ix-1/answer") {
		t.Error("a watcher that could not read the row says it tells the thread")
	}
	sw2 := r.sweeper(t, func(c *interact.SweepConfig) { c.Reopened = blind })
	if err := sw2.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
		t.Errorf("cancelled %v; want the task, on the sweeper's own reading of the row", got)
	}
}

// TestAPersonsAnswerThatCouldNotBeJudgedIsDroppedByTheSweep: the watcher could not read the row, so it kept the pause of an
// answer that is a person's. The sweeper reads the row, finds it answered, and cancels nothing and says nothing.
func TestAPersonsAnswerThatCouldNotBeJudgedIsDroppedByTheSweep(t *testing.T) {
	r := onlyApproval(t)
	if err := r.st.SubmitInteraction(t.Context(), "ix-1", []byte(`{}`), "UALICE1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	blind := interact.NewReopened(failingGet{r.st})
	blind.AnswerReopened(t.Context(), key, "task-1", "ix-1/answer")
	sw := r.sweeper(t, func(c *interact.SweepConfig) { c.Reopened = blind })
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.sess.cancelled(); len(got) != 0 || len(r.notices(t)) != 0 || sw.UndeliveredCount() != 0 {
		t.Errorf("cancelled %v, %d notices, %d held; want a person's answer left to the session, and nothing held", got, len(r.notices(t)), sw.UndeliveredCount())
	}
}

// TestTheNoticeOfADenialThatWasNotTakenIsGivenUpOn: five failed tries, as for the edit and the notice of a timeout. The cancel is
// made at the first, and the entry is dropped once the notice is given up on.
func TestTheNoticeOfADenialThatWasNotTakenIsGivenUpOn(t *testing.T) {
	r, sw := timedOutAndDenied(t)
	r.enq.n = 1000
	r.reopen(t)
	for i := 1; i <= 5; i++ {
		err := sw.Sweep(t.Context())
		if (err != nil) != (i < 5) {
			t.Fatalf("sweep %d = %v; want an error until the notice is given up on, at the fifth", i, err)
		}
	}
	if got := r.sess.cancelled(); len(got) != 1 || sw.UndeliveredCount() != 0 {
		t.Errorf("cancelled %v, %d held; want one cancel, and nothing held once the notice is given up on", got, sw.UndeliveredCount())
	}
	if !strings.Contains(r.logs.String(), "giving up on the notice of a denial that was not delivered") {
		t.Errorf("log %q; want the giving up said", r.logs.String())
	}
}

// TestADenialDroppedAsNotPausedIsNotRememberedAsTaken: ErrNotPaused sent nothing, so a later sweep of the row (the edit failed
// first) asks again, and the second is taken. Remembered as taken, the second sweep would not ask, and a denial that could be
// sent would not be.
func TestADenialDroppedAsNotPausedIsNotRememberedAsTaken(t *testing.T) {
	r := onlyApproval(t)
	r.sess.answerErrs = []error{session.ErrNotPaused} // not parked on the task yet
	r.upd.err = errBoom
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err == nil {
		t.Fatal("a sweep with a failing edit reported success")
	}
	if sw.TakenCount() != 0 {
		t.Error("a denial the session dropped as not paused is remembered as taken")
	}
	r.upd.mu.Lock()
	r.upd.err = nil
	r.upd.mu.Unlock()
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.sess.sent()); got != 2 {
		t.Errorf("%d denials asked of the session; want it asked again on the later sweep", got)
	}
	if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], deniedNotice) {
		t.Errorf("notices %q; want the denial the second ask took", notices)
	}
}

// TestADenialTheSessionTookIsForgottenWhenTheTimeoutIsRecordedOrDecidedOtherwise: what the sweeper remembers of a taken
// denial lives until its row is settled, by this sweep or by a person's answer in between.
func TestADenialTheSessionTookIsForgottenWhenTheTimeoutIsRecordedOrDecidedOtherwise(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(*sweepRig)
		state   store.InteractionState
	}{
		"recorded":                 {func(*sweepRig) {}, store.InteractionTimedOut},
		"answered in the meantime": {func(r *sweepRig) { r.st.conflict = true }, store.InteractionPending},
	} {
		t.Run(name, func(t *testing.T) {
			r := onlyApproval(t)
			tc.prepare(r)
			sw := r.sweeper(t, nil)
			if err := sw.Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.state(t, "ix-1") != tc.state || sw.TakenCount() != 0 {
				t.Errorf("state %v, %d remembered; want %v, and nothing remembered", r.state(t, "ix-1"), sw.TakenCount(), tc.state)
			}
		})
	}
	// While a later step fails the denial is remembered.
	r := onlyApproval(t)
	r.upd.err = errBoom
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err == nil || sw.TakenCount() != 1 {
		t.Errorf("Sweep = %v with %d remembered; want an error and the denial remembered until the timeout is recorded", err, sw.TakenCount())
	}
}

// TestATakenDenialStandsWhateverTheBindingSaysLater: the session took the denial, an edit failed, and by the next sweep the
// Binding is gone or its flag is off. The task is not cancelled for it and the thread is not told that a question was closed:
// the denial was sent, and the thread hears that it is being denied. The timeout is counted as an approval's, with the Binding.
func TestATakenDenialStandsWhateverTheBindingSaysLater(t *testing.T) {
	for name, change := range map[string]func(*sweepRig){
		"the Binding is gone":       func(r *sweepRig) { r.look.bindings = nil },
		"the flag is turned off":    flagFalse,
		"the thread has no session": func(r *sweepRig) { r.st.sessionErr = fmt.Errorf("%w", store.ErrNotFound) },
	} {
		t.Run(name, func(t *testing.T) {
			r := onlyApproval(t)
			r.upd.err = errBoom
			sw := r.sweeper(t, nil)
			if err := sw.Sweep(t.Context()); err == nil {
				t.Fatal("a sweep with a failing edit reported success")
			}
			r.upd.mu.Lock()
			r.upd.err = nil
			r.upd.mu.Unlock()
			change(r)
			if err := sw.Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := r.sess.cancelled(); len(got) != 0 || len(r.sess.sent()) != 1 {
				t.Errorf("cancelled %v, %d denials; want the denial that was sent to stand, and no cancel", got, len(r.sess.sent()))
			}
			if notices := r.noticeTexts(t); len(notices) != 1 || !strings.Contains(notices[0], deniedNotice) {
				t.Errorf("notices %q; want that the request is being denied", notices)
			}
			if !slices.Equal(r.outcomes(), []string{"ns/b timed_out"}) || r.state(t, "ix-1") != store.InteractionTimedOut {
				t.Errorf("counted %q, state %v; want it counted as an approval's, and timed out", r.outcomes(), r.state(t, "ix-1"))
			}
		})
	}
}

// TestAQuestionThatTimesOutIsNotTouchedByTheWatcher: a stray event for a question's ID changes nothing. Its task is cancelled
// by the timeout alone, once, as before.
func TestAQuestionThatTimesOutIsNotTouchedByTheWatcher(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	if r.reopened.AnswerReopened(t.Context(), key, "task-1", "ix-1/answer") {
		t.Error("the watcher says it tells the thread of a question")
	}
	sw := r.sweeper(t, nil)
	for range 3 {
		if err := sw.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.sess.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) || len(r.noticeTexts(t)) != 1 {
		t.Errorf("cancelled %v, %d notices; want the one cancel and the one notice of a question's timeout", got, len(r.noticeTexts(t)))
	}
}
