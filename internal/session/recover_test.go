package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	if len(sink.refs) != 1 || !sink.refs[0].Recovered || sink.refs[0].Key != key || sink.refs[0].Binding != "ns/b1" {
		t.Errorf("turn refs = %+v; want one, marked recovered, with the Binding of the thread (a pause found again needs it)", sink.refs)
	}
	if _, _, fo := f.agent.counts(); fo != 1 {
		t.Errorf("%d follows; want one", fo)
	}
}

// What recovery read about a conversation can be stale by the time it is acted on: the task it
// found may end, or be stopped, in between, and must then not be followed, or its end recorded
// over whatever the session holds now.
func TestRecoverDropsATaskThatEndedWhileItWasLookedAt(t *testing.T) {
	f, sink := restarted(t)
	var once sync.Once
	f.st.mu.Lock()
	f.st.onNext = func() {
		once.Do(func() { // a Stop lands while recovery reads the queue
			if err := f.st.Store.SetSessionTask(context.Background(), key, "c1", "", store.TaskTurn{}); err != nil {
				t.Error(err)
			}
		})
	}
	f.st.mu.Unlock()
	f.recover(t)
	drained := make(chan struct{})
	go func() { f.m.wg.Wait(); close(drained) }() // recovery and every runner it started are done
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery did not finish")
	}
	if _, _, fo := f.agent.counts(); fo != 0 {
		t.Errorf("%d follows; want none: the task was no longer recorded", fo)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 0 || len(sink.finals) != 0 {
		t.Errorf("%d turn refs, %d finals; want nothing rendered", len(sink.refs), len(sink.finals))
	}
}

// The re-read is a check, not a gate: if it cannot be made, the task is followed, as before. The
// agent knows whether the task still runs, and a task nobody follows is worse than one rendered twice.
func TestRecoverFollowsATaskWhenTheRecheckFails(t *testing.T) {
	f, sink := restarted(t)
	var once sync.Once
	f.st.mu.Lock()
	f.st.onNext = func() {
		once.Do(func() { // after recovery's own reads, before the runner's
			f.st.mu.Lock()
			f.st.fail["get"] = errors.New("database is locked")
			f.st.mu.Unlock()
		})
	}
	f.st.mu.Unlock()
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if _, _, fo := f.agent.counts(); fo != 1 {
		t.Errorf("%d follows; want the task followed once", fo)
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

// A pause that is found again after a restart, and then answered, continues under the answer: the paused turn is the
// recovered one, and its message (which the pause was shown in) is not the answer's output.
func TestRecoverOfAPausedTaskThatIsAnsweredRendersTheAnswerUnderItsOwnTurn(t *testing.T) {
	f, sink := restarted(t)
	recordedTurn(t, f, store.TaskTurn{ID: inbound("1").MessageID, User: "U1"})
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateInputRequired) }
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the runner to park", func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return f.m.convos[key] != nil && f.m.convos[key].parked == "t1"
	})
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 2 || sink.refs[0].MessageID != inbound("1").MessageID || sink.refs[1].MessageID != "ans-1" {
		t.Errorf("turn refs = %+v; want the pause under the recorded turn, then the answer's output under ans-1", sink.refs)
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
// touched back to life, one whose Binding left the config has nobody to ask, and one whose
// backend cannot be built says so under its own reason, with the error.
func TestRecoverSkipsWhatItCannotServe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding string
		age     time.Duration
		lookup  error // every Binding lookup fails with it, if set
		want    []string
	}{
		{"expired", "ns/b1", 2 * time.Hour, nil, []string{"skip=expired"}},
		{"unknown Binding", "ns/gone", 0, nil, []string{"skip=unknown_binding"}},
		{"a backend that cannot be built", "ns/b1", 0, errors.New("dialect unsupported"), []string{"skip=backend_error", "dialect unsupported"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := restarted(t)
			var logs bytes.Buffer // written by the recovery goroutine, read after it is done
			f.m.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			seeded := f.seed(tc.binding, tc.age)
			f.amu.Lock()
			f.bindingErr = tc.lookup
			f.amu.Unlock()
			f.recover(t)
			f.m.wg.Wait() // the recovery goroutines, and any runner they wrongly started, are done
			sess, err := f.st.GetSession(t.Context(), key, 1000*time.Hour, t0)
			if sess != seeded || err != nil && !errors.Is(err, store.ErrExpired) {
				t.Errorf("session = %+v, %v; want it untouched", sess, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log = %q; want it to say %q", logs.String(), want)
				}
			}
			if strings.Contains(logs.String(), "unknown_binding") != (tc.name == "unknown Binding") {
				t.Errorf("log = %q; unknown_binding is the reason for a Binding that left the config, and for nothing else", logs.String())
			}
			if sends, finds, follows := f.agent.counts(); sends+finds+follows != 0 || len(f.cancelled()) != 0 {
				t.Errorf("%d sends, %d lookups, %d follows, cancelled %v; want no call", sends, finds, follows, f.cancelled())
			}
		})
	}
}

// queuedBeforeTheRestart leaves turn 1 in the queue as a previous process did: enqueued, with no
// runner, and unknown to this process. The session is made if there is none.
func queuedBeforeTheRestart(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.session(); errors.Is(err, store.ErrNotFound) {
		if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", LastActivity: t0}); err != nil {
			t.Fatal(err)
		}
	}
	in := inbound("1")
	if _, err := f.st.EnqueueReply(t.Context(), store.Reply{ID: in.MessageID, Key: key, UserID: "U1", MessageRef: "1", Text: "hello", EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) queueEmpty() bool {
	_, err := f.st.NextReply(f.t.Context(), key)
	return errors.Is(err, store.ErrNotFound)
}

// A crash between recording the task and marking the turn delivered leaves a session with a
// task and a head that is that very task's turn. The history says so (FindTask): the head is
// marked delivered by following the task, and never sent again, or the turn would run twice.
func TestRecoverTakesTheHeadForTheRunningTurn(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(_, id string) (*a2a.Task, []a2a.TaskID, error) {
		if id != inbound("1").MessageID {
			t.Errorf("looked up %s; want the head", id)
		}
		return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
	}
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the head to be delivered", f.queueEmpty)
	if sends, _, _ := f.agent.counts(); sends != 0 {
		t.Errorf("%d sends; want none: the head is the running turn", sends)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) == 0 || sink.refs[0].MessageID != inbound("1").MessageID {
		t.Errorf("turn refs = %+v; want the output under the head's turn", sink.refs)
	}
}

// The turn that recovery takes the head for is never taken up from the queue again, only followed, so the
// reaction it may carry from before the crash comes off then: recovery marks it.
func TestRecoverTakesTheHeadsReactionOffWhenItFollowsItsTask(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
	}
	f.recover(t)
	waitFinal(t, sink)
	f.retired(t)
	if got, want := sink.dequeued(), []string{inbound("1").MessageID + "/queued 1"}; !slices.Equal(got, want) {
		t.Errorf("dequeued %q; want %q", got, want)
	}
}

// A head that is a later reply, queued behind the running task, waits for it, and is then looked
// up once more, as any turn that may have been sent before the crash, before it goes out.
func TestRecoverKeepsAHeadBehindTheRunningTask(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.recover(t)
	waitFinal(t, sink) // the task t1 ends
	waitFinal(t, sink) // then the head, sent
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if want := "[find follow find send]"; fmt.Sprint(f.agent.seq) != want {
		t.Errorf("calls = %v; want %s: the head is matched, the task followed, the head looked up, then sent once", f.agent.seq, want)
	}
}

// With no task recorded, a head of a conversation that has a context may have been sent before
// the crash: it is looked up, and followed if found, sent only if absent. A first turn has no
// context to look in, and is sent at once, for the agent deduplicates it by messageId.
func TestRecoverLooksUpAQueuedHeadBeforeSendingIt(t *testing.T) {
	setup := func(t *testing.T, noTask bool) (*fixture, *fakeSink) {
		f, sink := restarted(t)
		if noTask {
			if err := f.st.SetSessionTask(t.Context(), key, "", "", store.TaskTurn{}); err != nil { // context c1 stays
				t.Fatal(err)
			}
		}
		queuedBeforeTheRestart(t, f)
		return f, sink
	}
	t.Run("absent: sent", func(t *testing.T) {
		f, sink := setup(t, true)
		f.recover(t)
		waitFinal(t, sink)
		f.agent.mu.Lock()
		defer f.agent.mu.Unlock()
		if want := "[find send]"; fmt.Sprint(f.agent.seq) != want {
			t.Errorf("calls = %v; want %s", f.agent.seq, want)
		}
	})
	t.Run("found: followed, not sent", func(t *testing.T) {
		f, sink := setup(t, true)
		f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
			return &a2a.Task{ID: "t7", ContextID: "c1"}, []a2a.TaskID{"t7"}, nil
		}
		f.recover(t)
		waitFinal(t, sink)
		if sends, _, follows := f.agent.counts(); sends != 0 || follows != 1 {
			t.Errorf("%d sends, %d follows; want the found task followed and nothing sent", sends, follows)
		}
	})
	t.Run("a first turn, with no context: sent at once", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		queuedBeforeTheRestart(t, f) // Submit made the session, with no context
		f.recover(t)
		waitFinal(t, sink)
		if sends, finds, _ := f.agent.counts(); sends != 1 || finds != 0 {
			t.Errorf("%d sends, %d lookups; want one send and no lookup", sends, finds)
		}
	})
}

// A head that some other process enqueued may have been sent before it died, whichever wake gets
// to it first: Recover, or the queue handler's redelivery of the same event. So it is looked up
// before it goes into its context, in either order, and never sent blind.
func TestHeadFromBeforeTheRestartIsLookedUpWhateverWakesFirst(t *testing.T) {
	for name, order := range map[string]func(*testing.T, *fixture){
		"Recover, then the redelivered event at once": func(t *testing.T, f *fixture) {
			f.recover(t)
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
		},
		"the redelivered event, then Recover": func(t *testing.T, f *fixture) {
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			f.recover(t)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, sink := restarted(t)
			if err := f.st.SetSessionTask(t.Context(), key, "", "", store.TaskTurn{}); err != nil { // context c1 stays
				t.Fatal(err)
			}
			queuedBeforeTheRestart(t, f)
			order(t, f)
			waitFinal(t, sink)
			f.agent.mu.Lock()
			defer f.agent.mu.Unlock()
			// Recover may read the session once the turn has recorded its task, and look the head up
			// again: the first two calls are what matters, the lookup before the one send.
			if seq := f.agent.seq; len(seq) < 2 || seq[0] != "find" || seq[1] != "send" || len(f.agent.msgs) != 1 {
				t.Errorf("calls = %v, %d sends; want a lookup, then the one send", seq, len(f.agent.msgs))
			}
		})
	}
}

// A turn this process enqueued itself cannot have been sent before: it goes out with no lookup.
func TestHeadThisProcessEnqueuedIsSentWithoutALookup(t *testing.T) {
	f, sink := restarted(t)
	if err := f.st.SetSessionTask(t.Context(), key, "", "", store.TaskTurn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if sends, finds, _ := f.agent.counts(); sends != 1 || finds != 0 {
		t.Errorf("%d sends, %d lookups; want one send and no lookup", sends, finds)
	}
}

// The head counts as the running turn only if the task that holds it is the recorded one. One
// that another task holds is a turn behind t1, which is followed under its own turn.
func TestRecoverDoesNotTakeAHeadAnotherTaskHolds(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return &a2a.Task{ID: "t7", ContextID: "c1"}, []a2a.TaskID{"t7"}, nil
	}
	f.recover(t)
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) == 0 || sink.refs[0].MessageID != "task:t1" || !sink.refs[0].Recovered {
		t.Errorf("turn refs = %+v; want t1 followed under its own turn, task:t1", sink.refs)
	}
}

// recordedTurn leaves the session as a process that ended in the middle of a task left it: the task t1 is recorded, with the
// turn whose output it belongs to (SetSessionTask records the two in one write).
func recordedTurn(t *testing.T, f *fixture, turn store.TaskTurn) {
	t.Helper()
	if err := f.st.SetSessionTask(t.Context(), key, "c1", "t1", turn); err != nil {
		t.Fatal(err)
	}
}

// A task found again is rendered under the turn the session recorded for it, so that the renderer finds the message that
// turn began, a stream the leader that died left open included, and ends it and replaces it with the whole output, where a
// turn of its own would post the output once more beside it (spec 8.3, D37). The output is shown to the turn's author, which a
// stream needs.
func TestRecoverRendersATaskUnderTheTurnRecordedForIt(t *testing.T) {
	f, sink := restarted(t)
	recordedTurn(t, f, store.TaskTurn{ID: inbound("1").MessageID, User: "U1"})
	f.recover(t)
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || sink.refs[0].MessageID != inbound("1").MessageID || sink.refs[0].UserID != "U1" || !sink.refs[0].Recovered {
		t.Errorf("turn refs = %+v; want one, recovered, under the recorded turn (%s) and shown to its author, U1", sink.refs, inbound("1").MessageID)
	}
	eventually(t, "the task to be cleared", func() bool { return f.taskID() == "" })
	if sess, err := f.session(); err != nil || sess.Turn != (store.TaskTurn{}) {
		t.Errorf("session = %+v, %v; want the turn cleared with the task", sess, err)
	}
}

// A session with a task and no recorded turn (one that a binary from before the turn was recorded left) is followed under a
// turn of its own, which posts the output anew, as it was.
func TestRecoverOfATaskWithNoRecordedTurnPostsItsOutputUnderATurnOfItsOwn(t *testing.T) {
	f, sink := restarted(t)
	f.recover(t)
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || sink.refs[0].MessageID != "task:t1" || sink.refs[0].UserID != "" || !sink.refs[0].Recovered {
		t.Errorf("turn refs = %+v; want the task followed under task:t1, recovered", sink.refs)
	}
}

// A task that paused and was answered, and then is found again, continues under the ANSWER: its output is a message of its
// own, and the message of the turn that paused is not touched (the invariant of the runner's TurnRef). The session records
// the turn that began the task, and the answer's when the answer continues it, in the same write as the task.
func TestAnAnsweredTaskIsRecordedUnderTheAnswerAndRecoveredUnderIt(t *testing.T) {
	gate := make(chan struct{})
	f, sink := pausedFirst(t, func(_ *a2a.Message, h agent.Handler) error {
		accepted(h, 0) // the agent names the task again: the answer's turn is recorded
		<-gate
		return turn(h, 0, a2a.TaskStateCompleted)
	})
	defer close(gate)
	if sess, err := f.session(); err != nil || sess.TaskID != "t1" || sess.Turn.ID != inbound("1").MessageID || sess.Turn.User != "U1" {
		t.Fatalf("session while paused = %+v, %v; want t1 recorded under the turn that began it, %s, by U1", sess, err, inbound("1").MessageID)
	}
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer to be recorded as the turn of the task", func() bool {
		sess, err := f.session()
		return err == nil && sess.Turn.ID == "ans-1"
	})
	if sess, _ := f.session(); sess.Turn.User != "U1" || sess.TaskID != "t1" {
		t.Errorf("session = %+v; want task t1 under the answer, shown to U1", sess)
	}

	// The process ends here. A new one finds the session as it was left, and follows the task.
	rec, recSink := restarted(t)
	sess, err := f.session()
	if err != nil {
		t.Fatal(err)
	}
	recordedTurn(t, rec, sess.Turn)
	rec.recover(t)
	waitFinal(t, recSink)
	recSink.mu.Lock()
	defer recSink.mu.Unlock()
	if len(recSink.refs) != 1 || recSink.refs[0].MessageID != "ans-1" || recSink.refs[0].MessageID == inbound("1").MessageID || !recSink.refs[0].Recovered {
		t.Errorf("turn refs = %+v; want the continuation under the answer, ans-1, and never under the turn that paused (%s)", recSink.refs, inbound("1").MessageID)
	}
	_ = sink
}

// A turn recorded for another task is not this task's: a binary from before turns were recorded starts a task and leaves the
// session's turn as it was, and the output of the new task must not replace the message of the old one's turn.
func TestRecoverDoesNotTakeATurnThatWasRecordedForAnotherTask(t *testing.T) {
	f, sink := restarted(t)
	recordedTurn(t, f, store.TaskTurn{ID: inbound("1").MessageID, User: "U1"}) // recorded for t1
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, ContextID: "c1", TaskID: "t2", Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err) // the older binary began t2, and left the turn
	}
	f.recover(t)
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || sink.refs[0].MessageID != "task:t2" || sink.refs[0].UserID != "" {
		t.Errorf("turn refs = %+v; want t2 followed under a turn of its own, task:t2, which posts anew: the turn is t1's", sink.refs)
	}
}

// The answer's turn is recorded before the agent is asked, so a process that ends after the agent took the answer and before it
// said anything leaves the session naming the answer, and the recovery does not replace the message of the turn that paused. An
// answer that cannot be recorded is not sent: the pause stays open, and the person answers again.
func TestTheAnswersTurnIsRecordedBeforeTheAgentIsAsked(t *testing.T) {
	gate := make(chan struct{})
	f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error { <-gate; return nil }) // takes the answer, says nothing
	defer close(gate)

	f.st.fail = map[string]error{"set": errors.New("the database is busy")}
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the person to be told the answer was not delivered", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return slices.ContainsFunc(sink.notices, func(n Notice) bool { return n.Kind == NoticeFailed })
	})
	eventually(t, "the runner to park again", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.convos[key].parked == "t1" })
	f.agent.mu.Lock()
	sent := len(f.agent.answers)
	f.agent.mu.Unlock()
	if sent != 0 {
		t.Fatalf("%d answers reached the agent although the turn could not be recorded; want none", sent)
	}

	if err := f.m.Answer(t.Context(), key, answerMsg("ans-2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the agent to be asked", func() bool { f.agent.mu.Lock(); defer f.agent.mu.Unlock(); return len(f.agent.answers) == 1 })
	sess, err := f.session() // the agent has said nothing: no state event has been seen
	if err != nil || sess.TaskID != "t1" || sess.Turn.ID != "ans-2" || sess.TurnTask != "t1" {
		t.Fatalf("session = %+v, %v; want t1 under the answer, ans-2, before the agent has named the task", sess, err)
	}

	rec, recSink := restarted(t)
	recordedTurn(t, rec, sess.Turn)
	rec.recover(t)
	waitFinal(t, recSink)
	recSink.mu.Lock()
	defer recSink.mu.Unlock()
	if len(recSink.refs) != 1 || recSink.refs[0].MessageID != "ans-2" {
		t.Errorf("turn refs = %+v; want the continuation under the answer, ans-2, and not the turn that paused (%s)", recSink.refs, inbound("1").MessageID)
	}
}

// What is queued behind the task stays queued, and is no turn of the task: the output goes under the recorded turn.
func TestRecoverRendersTheTaskUnderItsRecordedTurnNotTheHeadBehindIt(t *testing.T) {
	f, sink := restarted(t)
	recordedTurn(t, f, store.TaskTurn{ID: inbound("1").MessageID, User: "U1"})
	if _, err := f.st.EnqueueReply(t.Context(), store.Reply{ID: inbound("2").MessageID, Key: key, UserID: "U1", MessageRef: "2", Text: "again", EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) { return nil, nil, nil }
	f.recover(t)
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) == 0 || sink.refs[0].MessageID != inbound("1").MessageID || !sink.refs[0].Recovered {
		t.Errorf("turn refs = %+v; want the task followed under the recorded turn %s, recovered", sink.refs, inbound("1").MessageID)
	}
}

// A head that is the task's own turn (a crash after the agent took it, before the queue knew) is the turn the output belongs
// to, and not whatever turn the session recorded before: here the session still names an earlier turn.
func TestRecoverRendersTheTaskUnderItsHeadWhenTheHeadIsItsOwnTurnAndNotTheEarlierRecordedOne(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	recordedTurn(t, f, store.TaskTurn{ID: "slack:slack:C1:0.5", User: "U9"}) // an earlier turn of the thread
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
	}
	f.recover(t)
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) == 0 || sink.refs[0].MessageID != inbound("1").MessageID {
		t.Errorf("turn refs = %+v; want the output under the head, %s, which is the task's own turn", sink.refs, inbound("1").MessageID)
	}
}

// If the lookup that matches the head to the running turn fails, recovery asks again with a
// backoff, and starts nothing meanwhile: a fallback would render the task's output under the wrong
// turn, and again under the right one.
func TestRecoverRetriesTheHeadMatch(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	lookups := 0
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		if lookups++; lookups <= 2 {
			return nil, nil, errors.New("agent unreachable")
		}
		return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
	}
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the head to be delivered", f.queueEmpty)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || sink.refs[0].MessageID != inbound("1").MessageID {
		t.Errorf("turn refs = %+v; want one, under the head's turn", sink.refs)
	}
}

// Recover may come after a wake: a Slack reply can reach the key first, and its runner hold the
// head behind the recorded task, busy in a lookup. The recovery is attached to that runner, which
// then follows the task (rendered once, recovered), and only after it is the held reply sent.
func TestRecoverAttachesToARunnerThatWokeFirst(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	inLookup, gate := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		if !first.Swap(true) {
			inLookup <- struct{}{}
			<-gate
		}
		return nil, nil, nil
	}
	if _, err := f.submit(inbound("1")); err != nil { // the redelivered event wakes a runner
		t.Fatal(err)
	}
	<-inLookup // the runner is busy holding the head behind t1
	f.recover(t)
	eventually(t, "the recovery to be attached to the busy runner", func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return f.m.convos[key].attach != nil // the runner is held in its lookup, so it has not taken it
	})
	close(gate)
	waitFinal(t, sink) // t1, followed
	waitFinal(t, sink) // then the held reply
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 2 || sink.refs[0].MessageID != "task:t1" || !sink.refs[0].Recovered || sink.refs[1].Recovered {
		t.Errorf("turn refs = %+v; want t1 recovered once, then the reply", sink.refs)
	}
	if sends, _, follows := f.agent.counts(); sends != 1 || follows != 1 {
		t.Errorf("%d sends, %d follows; want the reply sent once after t1 was followed", sends, follows)
	}
}
