package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// causeOf waits for the context the sink was given for turn i to end, and returns why it ended.
func (s *fakeSink) causeOf(t *testing.T, i int) error {
	t.Helper()
	var ctx context.Context
	eventually(t, "the sink's turn", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.ctxs) > i {
			ctx = s.ctxs[i]
		}
		return ctx != nil
	})
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("the context of turn %d is still open", i)
	}
	return context.Cause(ctx)
}

// The context a turn's sink is given ends with ErrTurnContinues when the turn's task goes on and a later turn renders it:
// a lost stream, which is followed again, and a shutdown, after which recovery follows it. A turn that ends for good, as one
// whose agent refused to continue does, ends it with any other cause, so the renderer ends what the turn showed.
func TestTheSinksContextSaysWhetherTheTurnContinues(t *testing.T) {
	t.Run("a lost stream is followed again", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
		f.agent.script = lost(errors.New("connection reset"))
		f.agent.get = working(t0)
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		if cause := sink.causeOf(t, 0); !errors.Is(cause, ErrTurnContinues) {
			t.Errorf("the lost send's sink context ended with %v; want ErrTurnContinues", cause)
		}
		if cause := sink.causeOf(t, 1); errors.Is(cause, ErrTurnContinues) {
			t.Errorf("the follow that saw the task end ended its sink context with %v; want the turn over", cause)
		}
	})
	t.Run("a send that never left is no follow", func(t *testing.T) {
		// The agent never had the turn, so nothing of it goes on: the send's sink context ends the turn, and the renderer
		// ends what it showed.
		f := newFixture(t)
		sink := f.live()
		f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
		f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
			if n == 0 {
				return fmt.Errorf("%w: the agent is down", agent.ErrNotSent)
			}
			return turn(h, n, a2a.TaskStateCompleted)
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		if cause := sink.causeOf(t, 0); cause == nil || errors.Is(cause, ErrTurnContinues) {
			t.Errorf("the sink context of a send that is tried again ended with %v; want the turn over", cause)
		}
	})
	t.Run("the agent refused to continue", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
			accepted(h, n)
			return errors.New("permanent 4xx")
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		if cause := sink.causeOf(t, 0); cause == nil || errors.Is(cause, ErrTurnContinues) {
			t.Errorf("the failed turn's sink context ended with %v; want the turn over", cause)
		}
	})
	t.Run("a shutdown leaves the task to recovery", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		named := make(chan struct{})
		f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
			accepted(h, n)
			close(named)
			<-ctx.Done()
			return ctx.Err()
		}
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		<-named
		_ = f.close()
		if cause := sink.causeOf(t, 0); !errors.Is(cause, ErrTurnContinues) {
			t.Errorf("the sink context of a turn cut by the shutdown ended with %v; want ErrTurnContinues", cause)
		}
	})
}

// A follow that ends without driving the task (the agent no longer has it, or it is cancelled as wedged, and the thread is
// told so) still gives the sink a turn, the one the lost stream handed its message on to: recovered, of the same message,
// and with a context that is already over and does not continue, so that the renderer ends what the lost stream showed.
func TestAFollowThatEndsWithoutDrivingEndsTheTurnItWasHanded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		get    func(f *fixture) func(a2a.TaskID) (*a2a.Task, error)
		notice NoticeKind
	}{
		{"the agent no longer has the task", func(*fixture) func(a2a.TaskID) (*a2a.Task, error) {
			return func(a2a.TaskID) (*a2a.Task, error) { return nil, a2a.NewError(a2a.ErrTaskNotFound, "gone") }
		}, NoticeSessionGone},
		{"the task is cancelled as wedged", func(f *fixture) func(a2a.TaskID) (*a2a.Task, error) {
			return func(a2a.TaskID) (*a2a.Task, error) {
				f.now.Store(t0.Add(2 * time.Hour).UnixNano())
				return nil, errors.New("agent unreachable")
			}
		}, NoticeWedged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
			f.agent.script = lost(errors.New("connection reset"))
			f.agent.get = tc.get(f)
			f.agent.follow = func(a2a.TaskID, agent.Handler) error { return errors.New("follow must not run") }
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the notice", func() bool { return len(sink.noticesOf(tc.notice)) == 1 })
			if cause := sink.causeOf(t, 0); !errors.Is(cause, ErrTurnContinues) {
				t.Errorf("the lost send's sink context ended with %v; want ErrTurnContinues", cause)
			}
			if cause := sink.causeOf(t, 1); errors.Is(cause, ErrTurnContinues) {
				t.Errorf("the follow's sink context ended with %v; want the turn over", cause)
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if len(sink.refs) != 2 || !sink.refs[1].Recovered || sink.refs[1].MessageID != sink.refs[0].MessageID {
				t.Errorf("sink turns %+v; want the send's and a recovered one of the same message", sink.refs)
			}
		})
	}
}

// A send whose outcome is unknown may have reached the agent, and the turn is tried again: looked up, then followed or sent
// once more, under the same message. Until the agent is known to have it or not, what the send showed is handed on, as a
// lost stream's is: its sink context ends with ErrTurnContinues, as does that of a later attempt that fails before the
// outcome is known, and the attempt after it is given the turn recovered, of the same message, so that it takes what was
// shown over rather than find it stopped and empty.
func TestASendOfUnknownOutcomeHandsItsMessageOn(t *testing.T) {
	unknown := func() error { return fmt.Errorf("%w: dial tcp: i/o timeout", agent.ErrOutcomeUnknown) }
	notSent := func() error { return fmt.Errorf("%w: connection refused", agent.ErrNotSent) }
	for _, tc := range []struct {
		name     string
		failures []func() error // the first sends fail like this, and the one after them works
	}{
		{"once", []func() error{unknown}},
		{"then not sent", []func() error{unknown, notSent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				if n < len(tc.failures) {
					return tc.failures[n]()
				}
				return turn(h, n, a2a.TaskStateCompleted)
			}
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			for i := range tc.failures {
				if cause := sink.causeOf(t, i); !errors.Is(cause, ErrTurnContinues) {
					t.Errorf("the sink context of failed send %d ended with %v; want ErrTurnContinues", i, cause)
				}
			}
			if cause := sink.causeOf(t, len(tc.failures)); errors.Is(cause, ErrTurnContinues) {
				t.Errorf("the sink context of the send that ended the task ended with %v; want the turn over", cause)
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if len(sink.refs) != len(tc.failures)+1 || sink.refs[0].Recovered {
				t.Fatalf("sink turns %+v; want the first send's, not recovered, and one per send after it", sink.refs)
			}
			for _, ref := range sink.refs[1:] {
				if !ref.Recovered || ref.MessageID != sink.refs[0].MessageID {
					t.Errorf("sink turns %+v; want every send after the first recovered, of the same message", sink.refs)
				}
			}
		})
	}
}

// A turn whose send's outcome was unknown, and that then ends without another sink turn (the lookup that has to come first
// fails until the turn is given up on, and the thread is told so), still ends what that send handed on: the sink is given the
// turn, recovered, of the same message, on a context that is already over and does not continue.
func TestATurnGivenUpOnAfterAnUnknownOutcomeEndsWhatItHandedOn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 2*time.Millisecond
	f.agent.script = unknown(1) // turn 2's send: turn 1 gave the session a context, so it is looked up before a resend
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		f.now.Add(int64(time.Hour)) // time passes, so that the turn is given up on
		return nil, nil, errors.New("agent unreachable")
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	if cause := sink.causeOf(t, 1); !errors.Is(cause, ErrTurnContinues) {
		t.Errorf("the sink context of the send of unknown outcome ended with %v; want ErrTurnContinues", cause)
	}
	if cause := sink.causeOf(t, 2); errors.Is(cause, ErrTurnContinues) {
		t.Errorf("the sink context of the turn given up on ended with %v; want the turn over", cause)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 3 || !sink.refs[2].Recovered || sink.refs[2].MessageID != sink.refs[1].MessageID {
		t.Errorf("sink turns %+v; want turn 1's, turn 2's send and a recovered one of turn 2's message", sink.refs)
	}
}

// A turn that an earlier process queued may have been sent by it, and that process may have left a message for the turn
// (a send of unknown outcome hands it on): this process's attempt is given the turn recovered, so that the renderer updates
// that message to what the attempt shows, rather than take the message the outbox returns for the content it was given.
func TestATurnAnEarlierProcessQueuedIsGivenRecovered(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	in := inbound("1")
	if _, err := f.st.EnqueueReply(t.Context(), store.Reply{ID: in.MessageID, Key: key, UserID: in.UserID, MessageRef: in.MessageRef, Text: in.Text, EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	f.m.wake(key) // the restarted process finds it in the queue
	waitFinal(t, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || !sink.refs[0].Recovered || sink.refs[0].MessageID != in.MessageID {
		t.Errorf("sink turns %+v; want one, recovered, of the turn's message", sink.refs)
	}
}
