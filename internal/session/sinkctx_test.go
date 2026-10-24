package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
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
	t.Run("a send that is tried again is no follow", func(t *testing.T) {
		// The resend is a turn of its own, not a recovered one, so it would not take over what this send showed: the send's
		// sink context ends the turn, and the renderer ends what it showed.
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
