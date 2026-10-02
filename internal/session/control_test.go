package session

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
)

// pauseFirst makes turn 0 pause for input on task t1 and every later turn complete.
func pauseFirst(_ context.Context, n int, h agent.Handler) error {
	if n == 0 {
		return turn(h, n, a2a.TaskStateInputRequired)
	}
	return turn(h, n, a2a.TaskStateCompleted)
}

// The stop control cancels the task that runs; the agent then ends the stream cancelled, and
// the queue goes on with the next reply (spec 8.2). The cancel is audited, under the person who
// pressed the button.
func TestStopCancelsARunningTaskAndTheQueueGoesOn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	mid, stopped := make(chan struct{}), make(chan struct{})
	f.agent.onCancel = func(a2a.TaskID) { close(stopped) }
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n > 0 {
			return turn(h, n, a2a.TaskStateCompleted)
		}
		id := accepted(h, n)
		mid <- struct{}{}
		<-stopped
		h(agent.FinalEvent{Task: &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}})
		return nil
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	<-mid
	if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || !did {
		t.Fatalf("Stop = %v, %v; want it to stop the task", did, err)
	}
	waitFinal(t, sink) // turn 1, cancelled
	waitFinal(t, sink) // turn 2
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel || f.audits[0].TaskID != "t1" || f.actors[0].User != "U9" || f.actors[0].Binding != "ns/b1" {
		t.Errorf("audits %+v by %+v; want the cancel of t1 by U9", f.audits, f.actors)
	}
	if got := sink.noticesOf(NoticeStopped); len(got) != 0 {
		t.Errorf("stopped notices = %+v; a running task's cancelled stream says it already", got)
	}
}

// A task that waits for input has no stream to end, so Stop ends the pause itself, tells the
// thread, and the held replies go out.
func TestStopEndsAPause(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = pauseFirst
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	waitFinal(t, sink)
	f.nextSent()
	if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || !did {
		t.Fatalf("Stop = %v, %v; want it to stop the task", did, err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the stop; want %s", id, want)
	}
	if got := sink.noticesOf(NoticeStopped); len(got) != 1 {
		t.Errorf("stopped notices = %+v; want one", got)
	}
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1", got)
	}
}

// With no task there is nothing to stop, and the agent is not asked.
func TestStopWithNoActiveTask(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || did || len(f.cancelled()) != 0 {
		t.Errorf("Stop = %v, %v, cancelled %v; want stopped=false and no cancel", did, err, f.cancelled())
	}
	other := Key{Connection: "slack", Channel: "C9", Thread: "1.1"}
	if did, err := f.m.Stop(t.Context(), other, "t1", "U9"); err != nil || did {
		t.Errorf("Stop of a thread nobody owns = %v, %v; want stopped=false", did, err)
	}
}

// Cancel is what the interaction timeout and an unusable pause call (spec 8.4). It works on a
// conversation with no runner, as after a restart: it cancels the task recorded, clears it, and
// starts the queue. It is audited, with no person named, and tells the thread nothing: the
// caller does. With no task recorded it says so.
func TestCancelWorksAfterARestart(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.seed("ns/b1", 0) // task t1, in context c1, recorded by a runner that is gone
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the held runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
	if err := f.m.Cancel(t.Context(), key, "t1", "interaction timed out"); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("1").MessageID; id != want {
		t.Errorf("sent %s after the cancel; want the held turn %s", id, want)
	}
	f.amu.Lock()
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel || f.audits[0].TaskID != "t1" || f.actors[0].User != "" {
		t.Errorf("audits %+v by %+v; want the cancel of t1, by no person", f.audits, f.actors)
	}
	f.amu.Unlock()
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if err := f.m.Cancel(t.Context(), key, "t1", "again"); !errors.Is(err, ErrNotPaused) {
		t.Errorf("Cancel with no task = %v; want ErrNotPaused", err)
	}
}

// pausedFirst sets a conversation up with turn 1 paused on task t1 and turn 2 queued behind
// it, and an agent whose answers run answer.
func pausedFirst(t *testing.T, answer func(*a2a.Message, agent.Handler) error) (*fixture, *fakeSink) {
	t.Helper()
	f := newFixture(t)
	sink := f.live()
	f.agent.script = pauseFirst
	f.agent.answer = answer
	f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) { return working(t0)(id) }
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	waitFinal(t, sink)
	f.nextSent()
	eventually(t, "the runner to park", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.convos[key].parked == "t1" })
	return f, sink
}

// answerMsg is the reply the dialect builds for the pause of task t1.
func answerMsg(id string) *a2a.Message {
	return &a2a.Message{ID: id, TaskID: "t1", Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart("PostgreSQL")}}
}

// The answer is sent to the paused task through the runner, which owns the stream: its events
// reach the sink under the turn that paused, and when the task ends, the held replies go out.
// Only a call for the task the conversation is paused on is accepted, once.
func TestAnswerResumesAPausedTask(t *testing.T) {
	f, sink := pausedFirst(t, func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) })
	for _, id := range []a2a.TaskID{"t9", ""} {
		msg := answerMsg("ans-1")
		msg.TaskID = id
		if err := f.m.Answer(t.Context(), key, msg); err == nil {
			t.Errorf("Answer for task %q was accepted; want it refused", id)
		}
	}
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-2")); !errors.Is(err, ErrNotPaused) {
		t.Errorf("a second answer = %v; want ErrNotPaused", err)
	}
	waitFinal(t, sink) // the answered task completes
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the answer; want the held reply %s", id, want)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if refs := sink.refs; len(refs) < 2 || refs[1].MessageID != "ans-1" || refs[1].Recovered {
		t.Errorf("turn refs = %+v; want the answer's output as a message of its own, named after the answer", refs)
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.answers) != 1 || f.agent.answers[0].ContextID != "c1" {
		t.Errorf("answers = %+v; want one, in the session's context", f.agent.answers)
	}
}

// Answer is refused when nothing is paused: no runner, or a task that is not the one paused.
func TestAnswerWithNothingPausedIsRefused(t *testing.T) {
	f := newFixture(t)
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); !errors.Is(err, ErrNotPaused) {
		t.Errorf("Answer with no conversation = %v; want ErrNotPaused", err)
	}
}

// An answer the agent rejects (-32603) wedges its task, which Agent.Answer cancels (R17): the
// thread is told, the call is audited, and the queue goes on.
func TestRejectedAnswerIsReportedAndTheQueueGoesOn(t *testing.T) {
	f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error {
		return fmt.Errorf("send message: %w", agent.ErrAnswerRejected)
	})
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the rejected answer; want %s", id, want)
	}
	if got := sink.noticesOf(NoticeAnswerRejected); len(got) != 1 || got[0].ID != "ans-1/answer_rejected" {
		t.Errorf("notices = %+v; want one for the answer", got)
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	// The answer's rejection, and the cancel Agent.Answer made of the wedged task, are two rows:
	// the cancel is a call courier's agent layer made, under no person.
	if len(f.audits) != 2 || f.audits[0].Op != audit.OpAnswer || !errors.Is(f.audits[0].Err, agent.ErrAnswerRejected) ||
		f.audits[1].Op != audit.OpCancel || f.audits[1].Err != nil || f.actors[1].User != "" {
		t.Errorf("audits = %+v by %+v; want the rejected answer, then the cancel, by no person", f.audits, f.actors)
	}
}

// An answer that the agent rejects after it names the task again is not a lost stream: kagent
// says -32603 over an SSE event that follows a WORKING update. The rejection is what happened, so
// the thread is told, the call is audited, the task is cleared and the held reply goes out.
func TestAnswerRejectedAfterTheTaskIsNamedAgainIsNotALostStream(t *testing.T) {
	f, sink := pausedFirst(t, func(_ *a2a.Message, h agent.Handler) error {
		h(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateWorking})
		return fmt.Errorf("send message: %w", agent.ErrAnswerRejected)
	})
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the rejected answer; want %s", id, want)
	}
	if got := sink.noticesOf(NoticeAnswerRejected); len(got) != 1 {
		t.Errorf("notices = %+v; want one", got)
	}
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) == 0 || f.audits[0].Op != audit.OpAnswer {
		t.Errorf("audits = %+v; want the rejected answer", f.audits)
	}
}

// If Agent.Answer's own cancel of the wedged task failed too (ErrCancelFailed), the task is still
// wedged: it stays recorded, and courier cancels it again, classified and bounded, until it is
// gone. Clearing it would leave every send getting -32004 for ever.
func TestWedgeWithAFailedAutoCancelIsCancelledAgain(t *testing.T) {
	f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error {
		return errors.Join(fmt.Errorf("send message: %w", agent.ErrAnswerRejected), fmt.Errorf("%w: down", agent.ErrCancelFailed))
	})
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.mu.Lock()
	f.agent.cancelErr = func(n int) error { // the first two tries fail, as the agent's own did
		if n <= 2 {
			return errors.New("agent unreachable")
		}
		return nil
	}
	f.agent.mu.Unlock()
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the wedge was cancelled; want %s", id, want)
	}
	if got := f.cancelled(); len(got) != 3 {
		t.Errorf("cancelled %v; want three tries of t1", got)
	}
	if got := sink.noticesOf(NoticeAnswerRejected); len(got) != 1 {
		t.Errorf("answer-rejected notices = %+v; want one", got)
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	for i, a := range f.audits {
		if a.Op == audit.OpCancel && (f.actors[i].User != "" || a.MessageID != "") { // courier's own cancels
			t.Errorf("cancel row %+v by %+v; want no person and no turn: courier made the cancel", a, f.actors[i])
		}
	}
}

// An answer whose outcome is unknown may have been delivered, so the task is read before anything
// is said or reparked: still waiting for input, it reopens; WORKING, it is followed; ended, the
// answer is dropped with a notice. A failure that never left keeps the plain reopening.
func TestAnswerOfUnknownOutcomeReadsTheTask(t *testing.T) {
	unknown := func(*a2a.Message, agent.Handler) error {
		return fmt.Errorf("%w: the stream ended", agent.ErrOutcomeUnknown)
	}
	state := func(s a2a.TaskState) func(a2a.TaskID) (*a2a.Task, error) {
		return func(id a2a.TaskID) (*a2a.Task, error) {
			return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: s, Timestamp: &t0}}, nil
		}
	}
	t.Run("still waiting: the pause reopens", func(t *testing.T) {
		f, _ := pausedFirst(t, unknown)
		f.agent.mu.Lock()
		f.agent.get = state(a2a.TaskStateInputRequired)
		f.agent.mu.Unlock()
		if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the pause to reopen", func() bool { return f.m.Answer(t.Context(), key, answerMsg("ans-2")) == nil })
	})
	t.Run("working: the answer went through, and the task is followed", func(t *testing.T) {
		f, sink := pausedFirst(t, unknown)
		f.agent.mu.Lock()
		f.agent.get = state(a2a.TaskStateWorking)
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
		f.agent.mu.Unlock()
		if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
			t.Fatal(err)
		}
		if id, want := f.nextSent(), inbound("2").MessageID; id != want {
			t.Errorf("sent %s; want the held reply %s", id, want)
		}
		if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
			t.Errorf("failed notices = %+v; want none: the answer was delivered", got)
		}
	})
	t.Run("ended: the answer is dropped", func(t *testing.T) {
		f, sink := pausedFirst(t, unknown)
		f.agent.mu.Lock()
		f.agent.get = state(a2a.TaskStateCompleted)
		f.agent.mu.Unlock()
		if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeAnswerDropped)) == 1 })
		if id, want := f.nextSent(), inbound("2").MessageID; id != want {
			t.Errorf("sent %s; want the held reply %s", id, want)
		}
	})
}

// -32004 on an answer means busy, wedged, or ended. The task is read: ended, and the answer is
// dropped with a notice; WORKING past maxWorkingAge, and it is cancelled (R17); otherwise it is
// busy with something else: the thread is told, and the task is followed, so its end is seen and
// the queue goes on, not parked on a task that runs.
func TestBusyAnswerReadsTheTask(t *testing.T) {
	busy := func(*a2a.Message, agent.Handler) error { return fmt.Errorf("send message: %w", agent.ErrBusy) }
	ended := func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
	}
	for _, tc := range []struct {
		name   string
		get    func(a2a.TaskID) (*a2a.Task, error)
		notice NoticeKind
		cancel bool // and the queue goes on
	}{
		{"the task ended", ended, NoticeAnswerDropped, false},
		{"wedged past maxWorkingAge", working(t0.Add(-2 * time.Hour)), NoticeWedged, true},
		{"busy for now: it is followed, to its end", working(t0.Add(2 * time.Hour)), NoticeFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := pausedFirst(t, busy)
			f.agent.mu.Lock()
			f.agent.get = tc.get
			f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
			f.agent.mu.Unlock()
			f.now.Store(t0.Add(2 * time.Hour).UnixNano())
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the notice", func() bool { return len(sink.noticesOf(tc.notice)) == 1 })
			if got := len(f.cancelled()) == 1; got != tc.cancel {
				t.Errorf("cancelled %v; want a cancel: %v", f.cancelled(), tc.cancel)
			}
			if id, want := f.nextSent(), inbound("2").MessageID; id != want {
				t.Errorf("sent %s; want the held reply %s", id, want)
			}
		})
	}
}

// An answer that could not be sent leaves the task paused: the thread is told, and it can be
// answered again. A refused credential also goes to the identity Source.
func TestAnswerThatCannotBeSentLeavesThePauseOpen(t *testing.T) {
	for name, cause := range map[string]error{
		"agent unreachable":   fmt.Errorf("%w: down", agent.ErrNotSent),
		"credentials refused": fmt.Errorf("%w: %w", agent.ErrNotSent, agent.ErrCredentialsRejected),
	} {
		t.Run(name, func(t *testing.T) {
			f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error { return cause })
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
			eventually(t, "the pause to reopen", func() bool { return f.m.Answer(t.Context(), key, answerMsg("ans-2")) == nil })
			f.amu.Lock()
			defer f.amu.Unlock()
			if errors.Is(cause, agent.ErrCredentialsRejected) != (len(f.rejected) >= 1) {
				t.Errorf("Rejected calls = %+v for %v", f.rejected, cause)
			}
		})
	}
}

// A control names the task it was rendered for. One that arrives late, for a task that has
// ended, must leave the newer task alone: a stale timeout or an old button cancels nothing.
func TestStaleControlsLeaveANewerTaskAlone(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n < 2 {
			return turn(h, n, a2a.TaskStateInputRequired) // t1, then t2, each paused
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	waitFinal(t, sink)
	if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || !did {
		t.Fatalf("Stop(t1) = %v, %v", did, err)
	}
	waitFinal(t, sink) // turn 2 pauses on t2
	if err := f.m.Cancel(t.Context(), key, "t1", "interaction timed out"); !errors.Is(err, ErrNotPaused) {
		t.Errorf("late Cancel of t1 = %v; want ErrNotPaused", err)
	}
	if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || did {
		t.Errorf("late Stop of t1 = %v, %v; want stopped=false", did, err)
	}
	if got, recorded := f.cancelled(), f.taskID(); len(got) != 1 || got[0] != "t1" || recorded != "t2" {
		t.Errorf("cancelled %v, recorded %q; want only t1 cancelled, and t2 still recorded", got, recorded)
	}
}

// A runner that exists but is not driving the task (a turn held behind a task recorded before a
// restart, here inside its lookup) does not end the task by itself. Stop cancels it, clears the
// record if it is still the recorded one, and wakes the runner, so the held turn goes out.
func TestStopOfATaskNoRunnerDrivesClearsItAndWakesTheQueue(t *testing.T) {
	f := newFixture(t)
	f.live()
	f.seed("ns/b1", 0) // task t1 recorded, in context c1
	inLookup, gate := make(chan struct{}), make(chan struct{})
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		inLookup <- struct{}{}
		<-gate
		return nil, nil, nil
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-inLookup // the runner exists, and drives nothing
	if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || !did {
		t.Fatalf("Stop = %v, %v; want it to stop the task", did, err)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) { return nil, nil, nil }
	close(gate)
	if id, want := f.nextSent(), inbound("1").MessageID; id != want {
		t.Errorf("sent %s; want the held turn %s", id, want)
	}
}

// A cancel that fails is classified the way a wedged task's is: a task that ended meanwhile is
// as good as cancelled; a task the agent no longer knows resets the session; a refused
// credential goes to the identity Source; a refused identity is audited; a failure that may pass
// is retried a few times. Every answer is audited once, and only the ones that end the task end
// the pause.
func TestFailedStopIsClassified(t *testing.T) {
	ended := func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
	}
	for _, tc := range []struct {
		name     string
		cause    error
		wantStop bool // Stop reports the task stopped
		wantErr  error
		notice   NoticeKind // besides the stop's own
		rows     int        // audit rows the manager writes
		sources  int        // Rejected calls
		attempts int
	}{
		{"it ended meanwhile", fmt.Errorf("cancel: %w", a2a.ErrTaskNotCancelable), true, nil, NoticeStopped, 1, 0, 1},
		{"the agent no longer knows it", a2a.NewError(a2a.ErrTaskNotFound, "gone"), true, nil, NoticeSessionGone, 1, 0, 1},
		{"credentials refused", fmt.Errorf("%w: rejected", agent.ErrCredentialsRejected), false, agent.ErrCredentialsRejected, "", 0, 1, 1},
		{"identity refused", fmt.Errorf("send: %w", agent.ErrForbidden), false, agent.ErrForbidden, "", 1, 0, 1},
		{"it may pass, and does not", errors.New("agent unreachable"), false, nil, "", 1, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := pausedFirst(t, nil)
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			f.agent.mu.Lock()
			f.agent.get = ended
			f.agent.cancelErr = func(int) error { return tc.cause }
			f.agent.mu.Unlock()
			did, err := f.m.Stop(t.Context(), key, "t1", "U9")
			if did != tc.wantStop || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || (tc.wantErr == nil && tc.name != "it may pass, and does not" && err != nil) {
				t.Errorf("Stop = %v, %v; want stopped=%v and %v", did, err, tc.wantStop, tc.wantErr)
			}
			if tc.name == "it may pass, and does not" && err == nil {
				t.Error("Stop after a cancel that never works = nil; want an error")
			}
			if got := len(f.cancelled()); got != tc.attempts {
				t.Errorf("%d cancel attempts; want %d", got, tc.attempts)
			}
			if tc.notice != "" && len(sink.noticesOf(tc.notice)) != 1 {
				t.Errorf("%s notices = %+v; want one", tc.notice, sink.noticesOf(tc.notice))
			}
			f.amu.Lock()
			rows, sources := len(f.audits), len(f.rejected)
			f.amu.Unlock()
			if rows != tc.rows || sources != tc.sources {
				t.Errorf("%d audit rows, %d Rejected calls; want %d and %d", rows, sources, tc.rows, tc.sources)
			}
			if tc.wantStop { // the pause ended: the held reply goes out
				if id, want := f.nextSent(), inbound("2").MessageID; id != want {
					t.Errorf("sent %s after the stop; want the held reply %s", id, want)
				}
				if tc.notice == NoticeSessionGone { // the session was reset: a fresh context, and one notice for the task
					f.agent.mu.Lock()
					last := f.agent.msgs[len(f.agent.msgs)-1]
					f.agent.mu.Unlock()
					if got := sink.noticesOf(NoticeSessionGone); last.ContextID != "" || len(got) != 1 || got[0].ID != "task:t1/session_gone" {
						t.Errorf("held reply in context %q, notices %+v; want no context and the notice task:t1/session_gone", last.ContextID, got)
					}
				}
			}
		})
	}
}

// An exit that leaves a task recorded, while the runner stays alive behind it, must not leave the
// runner looking as if it still drove the task: Stop then clears the record, and the held turn
// goes out. The exits of a follow (a cancel that failed for good) and of a stream (a turn that
// could not be recorded) each have their own way of leaving it.
func TestStopAfterARunnerExitThatLeftTheTaskRecorded(t *testing.T) {
	lookupsFailWhileRecorded := func(f *fixture) func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return func(string, string) (*a2a.Task, []a2a.TaskID, error) {
			if f.taskID() != "" {
				return nil, nil, errors.New("agent unreachable") // the held turn waits, and the runner lives
			}
			return nil, nil, nil
		}
	}
	t.Run("a wedged task whose cancel was refused", func(t *testing.T) {
		f, sink := wedgedFixture(t, func(f *fixture) {
			f.agent.cancelErr = func(n int) error {
				if n == 1 {
					return fmt.Errorf("send: %w", agent.ErrForbidden)
				}
				return nil
			}
			f.agent.find = lookupsFailWhileRecorded(f)
		}, "1", "2")
		eventually(t, "the cancel to be refused for good", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		f.nextSent() // turn 1
		if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || !did {
			t.Fatalf("Stop = %v, %v", did, err)
		}
		if id, want := f.nextSent(), inbound("2").MessageID; id != want {
			t.Errorf("sent %s after the stop; want the held turn %s", id, want)
		}
	})
	t.Run("a turn that could not be recorded", func(t *testing.T) {
		f := newFixture(t)
		f.live()
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
		f.st.stuck["mark"] = errors.New("disk full")
		f.agent.find = lookupsFailWhileRecorded(f)
		gate := make(chan struct{})
		f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
			<-gate
			return turn(h, n, a2a.TaskStateCompleted)
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		f.nextSent()
		if _, err := f.submit(inbound("1")); err != nil { // a wake while the turn runs keeps the runner alive through its failure
			t.Fatal(err)
		}
		close(gate)
		eventually(t, "the runner to wait on the failing lookup", func() bool { _, finds, _ := f.agent.counts(); return finds > 0 })
		f.st.mu.Lock()
		delete(f.st.stuck, "mark") // the store recovers
		f.st.mu.Unlock()
		if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err != nil || !did {
			t.Fatalf("Stop = %v, %v", did, err)
		}
		if id, want := f.nextSent(), inbound("1").MessageID; id != want {
			t.Errorf("sent %s after the stop; want the turn, looked up and found absent: %s", id, want)
		}
	})
}

// Cancel is for a pause. A task that streams is not one, and is left alone.
func TestCancelRefusesAStreamingTask(t *testing.T) {
	f := newFixture(t)
	f.live()
	mid, gate := make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		mid <- struct{}{}
		<-gate
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-mid
	if err := f.m.Cancel(t.Context(), key, "t1", "interaction timed out"); !errors.Is(err, ErrNotPaused) || len(f.cancelled()) != 0 {
		t.Errorf("Cancel of a streaming task = %v, cancelled %v; want ErrNotPaused and no cancel", err, f.cancelled())
	}
	close(gate)
}

// -32002 says the task could not be cancelled, which is fine if it ended meanwhile and not if it
// is still active, or cannot be read: an error then, with the pause still held.
func TestStopWhenTheTaskCannotBeCancelledAndIsNotOver(t *testing.T) {
	for name, get := range map[string]func(a2a.TaskID) (*a2a.Task, error){
		"still waiting for input": func(id a2a.TaskID) (*a2a.Task, error) {
			return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
		},
		"it cannot be read": func(a2a.TaskID) (*a2a.Task, error) { return nil, errors.New("agent unreachable") },
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := pausedFirst(t, func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) })
			f.agent.mu.Lock()
			f.agent.get = get
			f.agent.cancelErr = func(int) error { return fmt.Errorf("cancel: %w", a2a.ErrTaskNotCancelable) }
			f.agent.mu.Unlock()
			if did, err := f.m.Stop(t.Context(), key, "t1", "U9"); err == nil || did {
				t.Errorf("Stop = %v, %v; want an error", did, err)
			}
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil { // the pause is still held
				t.Errorf("Answer after the failed stop = %v; want the pause still open", err)
			}
		})
	}
}

// A turn that ends leaving its task recorded tells the thread before the runner has finished
// with it. A Stop that arrives in that moment must not take the runner for still driving the
// task: it would cancel the task and leave the record, and the turns held behind it, for good.
// The runner stops driving the task before it says so.
func TestStopWhileTheRunnerIsTellingTheThreadItGaveUp(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f, _ := wedgedFixture(t, func(f *fixture) {
		f.agent.cancelErr = func(n int) error {
			if n == 1 {
				return fmt.Errorf("send: %w", agent.ErrForbidden) // the runner's own cancel of the wedged task
			}
			return nil
		}
		f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
			if f.taskID() != "" {
				return nil, nil, errors.New("agent unreachable") // the held turn waits, and the runner lives
			}
			return nil, nil, nil
		}
		f.m.cfg.Sink.(*fakeSink).onNotice = func(n Notice) { // the runner is saying the cancel was refused, and is held here
			if n.Kind == NoticeFailed && !first.Swap(true) {
				close(entered)
				<-release
			}
		}
	}, "1")
	<-entered
	did, err := f.m.Stop(t.Context(), key, "t1", "U9")
	if recorded := f.taskID(); err != nil || !did || recorded != "" {
		t.Errorf("Stop = %v, %v; task %q; want it stopped and the record cleared, whatever the runner is doing", did, err, recorded)
	}
	close(release)
}
