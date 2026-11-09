package session

import (
	"context"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// A pause whose Binding has left the config cannot be cancelled: the cancel needs the Binding's backend, and Cancel fails for as long as
// the Binding is missing. Abandon ends the pause without the agent. The runner parked on the task is released (so HoldsTask is false and
// retention may deal with the session), the task is cleared from the session, and the agent is not called. The turn held behind the pause is
// dropped with the notice for a Binding that is gone, as any turn of such a Binding is.
func TestAbandonReleasesAParkedRunnerOfABindingThatIsGone(t *testing.T) {
	f, sink := pausedFirst(t, nil)
	f.amu.Lock()
	f.bindingErr = ErrUnknownBinding
	f.amu.Unlock()

	if err := f.m.Cancel(t.Context(), key, "t1", "interaction timed out"); !errors.Is(err, ErrUnknownBinding) {
		t.Fatalf("Cancel of a Binding that is gone = %v; want an error wrapping ErrUnknownBinding, the failure Abandon is for", err)
	}
	if !f.m.HoldsTask(key, "t1") {
		t.Fatal("the runner does not hold the task after a Cancel that failed")
	}

	if err := f.m.Abandon(t.Context(), key, "t1", "its Binding is not in the config"); err != nil {
		t.Fatalf("Abandon = %v; want the pause ended", err)
	}
	eventually(t, "the runner to let go of the task", func() bool { return !f.m.HoldsTask(key, "t1") })
	eventually(t, "the task to be cleared from the session", func() bool { return f.taskID() == "" })
	if got := f.cancelled(); len(got) != 0 {
		t.Errorf("the agent was asked to cancel %v; want no call: Abandon never reaches the agent", got)
	}
	f.amu.Lock()
	calls := len(f.audits)
	f.amu.Unlock()
	if calls != 0 {
		t.Errorf("%d agent calls audited; want none: nothing was sent to the agent", calls)
	}
	eventually(t, "the held turn to be dropped with the notice for a Binding that is gone", func() bool { return len(sink.noticesOf(NoticeBindingGone)) == 1 })
	if err := f.m.Abandon(t.Context(), key, "t1", "again"); !errors.Is(err, ErrNotPaused) {
		t.Errorf("a second Abandon = %v; want ErrNotPaused", err)
	}
}

// Abandon is for the pause of the task named. A newer task, a thread nobody owns, a task that streams and one whose pause another control
// has closed are left alone.
func TestAbandonLeavesWhatIsNotThatPauseAlone(t *testing.T) {
	t.Run("another task", func(t *testing.T) {
		f, _ := pausedFirst(t, nil)
		if err := f.m.Abandon(t.Context(), key, "t2", "late"); !errors.Is(err, ErrNotPaused) {
			t.Errorf("Abandon of a task that is not the recorded one = %v; want ErrNotPaused", err)
		}
		if !f.m.HoldsTask(key, "t1") || f.taskID() != "t1" {
			t.Error("the pause of the recorded task was ended by an Abandon of another")
		}
	})
	t.Run("another thread", func(t *testing.T) {
		f, _ := pausedFirst(t, nil)
		other := Key{Connection: key.Connection, Channel: key.Channel, Thread: "9.9"}
		if err := f.m.Abandon(t.Context(), other, "t1", "late"); !errors.Is(err, ErrNotPaused) {
			t.Errorf("Abandon in a thread with no session = %v; want ErrNotPaused", err)
		}
		if !f.m.HoldsTask(key, "t1") {
			t.Error("the pause of another thread was ended")
		}
	})
	t.Run("a task that streams", func(t *testing.T) {
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
		if err := f.m.Abandon(t.Context(), key, "t1", "late"); !errors.Is(err, ErrNotPaused) {
			t.Errorf("Abandon of a task that streams = %v; want ErrNotPaused", err)
		}
		close(gate)
	})
}

// After a restart there is no runner, only the session's record: Abandon clears it and starts the queue, as Cancel does, with no call to
// the agent.
func TestAbandonWorksAfterARestart(t *testing.T) {
	f := newFixture(t)
	f.live()
	f.seed("ns/b1", 0) // task t1, in context c1, recorded by a runner that is gone
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the held runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
	if err := f.m.Abandon(t.Context(), key, "t1", "its Binding is not in the config"); err != nil {
		t.Fatalf("Abandon = %v", err)
	}
	if id, want := f.nextSent(), inbound("1").MessageID; id != want {
		t.Errorf("sent %s after the Abandon; want the held turn %s", id, want)
	}
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if got := f.cancelled(); len(got) != 0 {
		t.Errorf("the agent was asked to cancel %v; want no call", got)
	}
}
