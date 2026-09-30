package session

import (
	"context"
	"errors"
	"testing"

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
