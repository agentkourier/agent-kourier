package session

import (
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// restarted is a conversation as a restart finds it: the session records task t1 in context c1,
// and no runner exists. The agent's follow replays what the task has said.
func restarted(t *testing.T) (*fixture, *fakeSink) {
	t.Helper()
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.seed("ns/b1", 0)
	f.agent.get = working(t0)
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
	return f, sink
}

func (f *fixture) recover(t *testing.T) {
	t.Helper()
	if err := f.m.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// A task that still runs is followed where it stands (spec 8.3), and its output reaches the
// sink marked as recovered. Recover returns at once, and a second call starts nothing: the
// conversation already has its runner.
func TestRecoverFollowsARunningTask(t *testing.T) {
	f, sink := restarted(t)
	gate := make(chan struct{})
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
		<-gate // the task keeps running
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	f.recover(t) // returns while the task is still followed
	f.recover(t)
	close(gate)
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || !sink.refs[0].Recovered || sink.refs[0].Key != key {
		t.Errorf("turn refs = %+v; want one, marked recovered", sink.refs)
	}
	if _, _, fo := f.agent.counts(); fo != 1 {
		t.Errorf("%d follows; want one", fo)
	}
}

// A task that ended while courier was down has its final output rendered, once, and is cleared.
func TestRecoverRendersAFinishedTaskOnce(t *testing.T) {
	f, sink := restarted(t)
	f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
	}
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	f.recover(t) // the session no longer lists: nothing is rendered again
	f.retired(t)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if finals := len(sink.finals); finals != 0 || len(sink.refs) != 1 {
		t.Errorf("%d unread finals, %d turn refs; want the output rendered once", finals, len(sink.refs))
	}
}

// A task that waits for input parks the conversation again, and its pause is shown again (8.4):
// the answer, or a stop, then works as before the restart.
func TestRecoverParksAPausedTask(t *testing.T) {
	f, sink := restarted(t)
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateInputRequired) }
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
	f.recover(t)
	f.recover(t)
	waitFinal(t, sink) // the pause, shown again
	eventually(t, "the runner to park", func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return f.m.convos[key] != nil && f.m.convos[key].parked == "t1"
	})
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if _, _, fo := f.agent.counts(); fo != 1 {
		t.Errorf("%d follows; want the second Recover to start nothing", fo)
	}
}

// A task WORKING past maxWorkingAge that nobody has heard of since is presumed wedged, and
// cancelled through the same classified cancel as any other (R17): a wedge looks like a long run
// after a crash.
func TestRecoverCancelsAWedgedTask(t *testing.T) {
	f, sink := restarted(t)
	f.agent.get = working(t0.Add(-2 * time.Hour))
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return errors.New("follow must not run") }
	f.recover(t)
	eventually(t, "the wedged notice", func() bool { return len(sink.noticesOf(NoticeWedged)) == 1 })
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1", got)
	}
}

// A task the agent does not know any more is a lost session: the thread is told, and the next
// turn starts fresh.
func TestRecoverOfAGoneTaskResetsTheSession(t *testing.T) {
	f, sink := restarted(t)
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, a2a.NewError(a2a.ErrTaskNotFound, "gone") }
	f.recover(t)
	eventually(t, "the session to be reset", func() bool {
		sess, _ := f.session()
		return sess.ContextID == "" && sess.TaskID == "" && len(sink.noticesOf(NoticeSessionGone)) == 1
	})
}

// A backend that is down at startup does not stop recovery: it is asked again, with a backoff,
// and Recover has already returned.
func TestRecoverRetriesWhileTheAgentIsDown(t *testing.T) {
	f, sink := restarted(t)
	tries := 0
	f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) {
		if tries++; tries <= 3 {
			return nil, errors.New("agent unreachable")
		}
		return working(t0)(id)
	}
	f.recover(t)
	waitFinal(t, sink)
}

// What recovery cannot serve it leaves as it found it: a session past its threadTTL is never
// touched back to life, and one whose Binding left the config has nobody to ask.
func TestRecoverSkipsExpiredSessionsAndUnknownBindings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding string
		age     time.Duration
	}{
		{"expired", "ns/b1", 2 * time.Hour},
		{"unknown Binding", "ns/gone", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := restarted(t)
			seeded := f.seed(tc.binding, tc.age)
			f.recover(t)
			f.m.wg.Wait() // the recovery goroutines, and any runner they wrongly started, are done
			sess, err := f.st.GetSession(t.Context(), key, 1000*time.Hour, t0)
			if sess != seeded || err != nil && !errors.Is(err, store.ErrExpired) {
				t.Errorf("session = %+v, %v; want it untouched", sess, err)
			}
			if sends, finds, follows := f.agent.counts(); sends+finds+follows != 0 || len(f.cancelled()) != 0 {
				t.Errorf("%d sends, %d lookups, %d follows, cancelled %v; want no call", sends, finds, follows, f.cancelled())
			}
		})
	}
}
