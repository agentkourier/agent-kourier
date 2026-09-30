package session

import (
	"context"
	"errors"
	"fmt"
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
	if did, err := f.m.Stop(t.Context(), key, "U9"); err != nil || !did {
		t.Fatalf("Stop = %v, %v; want it to stop the task", did, err)
	}
	<-sink.finals // turn 1, cancelled
	<-sink.finals // turn 2
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
	<-sink.finals
	f.nextSent()
	if did, err := f.m.Stop(t.Context(), key, "U9"); err != nil || !did {
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
	<-sink.finals
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if did, err := f.m.Stop(t.Context(), key, "U9"); err != nil || did || len(f.cancelled()) != 0 {
		t.Errorf("Stop = %v, %v, cancelled %v; want stopped=false and no cancel", did, err, f.cancelled())
	}
	other := Key{Connection: "slack", Channel: "C9", ThreadTS: "1.1"}
	if did, err := f.m.Stop(t.Context(), other, "U9"); err != nil || did {
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
	if err := f.m.Cancel(t.Context(), key, "interaction timed out"); err != nil {
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
	<-sink.finals
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if err := f.m.Cancel(t.Context(), key, "again"); !errors.Is(err, ErrNotPaused) {
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
	<-sink.finals
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
	<-sink.finals // the answered task completes
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the answer; want the held reply %s", id, want)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if refs := sink.refs; len(refs) < 2 || refs[1].MessageID != inbound("1").MessageID {
		t.Errorf("turn refs = %+v; want the answer's output under turn 1", refs)
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
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpAnswer || !errors.Is(f.audits[0].Err, agent.ErrAnswerRejected) {
		t.Errorf("audits = %+v; want the answer, rejected", f.audits)
	}
}

// -32004 on an answer means busy, wedged, or ended. The task is read: ended, and the answer is
// dropped with a notice; WORKING past maxWorkingAge, and it is cancelled (R17); otherwise it is
// busy with something else, the thread is told, and the pause stays open for another try.
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
		open   bool // the pause stays open
	}{
		{"the task ended", ended, NoticeAnswerDropped, false, false},
		{"wedged past maxWorkingAge", working(t0.Add(-2 * time.Hour)), NoticeWedged, true, false},
		{"busy for now", working(t0), NoticeFailed, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := pausedFirst(t, busy)
			f.agent.get = tc.get
			f.now.Store(t0.Add(30 * time.Minute).UnixNano())
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the notice", func() bool { return len(sink.noticesOf(tc.notice)) == 1 })
			if got := len(f.cancelled()) == 1; got != tc.cancel {
				t.Errorf("cancelled %v; want a cancel: %v", f.cancelled(), tc.cancel)
			}
			if tc.open { // the same task may be answered again
				eventually(t, "the pause to reopen", func() bool {
					return f.m.Answer(t.Context(), key, answerMsg("ans-2")) == nil
				})
				return
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
