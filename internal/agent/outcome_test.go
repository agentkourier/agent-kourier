package agent

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// A WORKING task that streams nothing must not keep Send resubscribing for ever: the
// snapshot each SubscribeToTask opens with changes nothing, so it is not progress.
func TestSilentWorkingTaskFailsAfterMaxReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, sub1, sub2, sub3 := make(chan item), make(chan item), make(chan item), make(chan item)
		m := newMemConn(t, []chan item{send}, []chan item{sub1, sub2, sub3}, getResult{task: task(a2a.TaskStateWorking, "")})
		s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 3 }))
		s.feed(send, item{ev: working()})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		for _, sub := range []chan item{sub1, sub2, sub3} {
			s.feed(sub, item{ev: task(a2a.TaskStateWorking, "")}) // the opening snapshot, then silence
		}
		err := s.result()
		var lost *StreamLostError
		if !errors.Is(err, ErrStreamLost) || !errors.As(err, &lost) || lost.TaskID != "t1" || lost.ContextID != "c1" {
			t.Fatalf("err = %v, want a StreamLostError for t1 in c1", err)
		}
		if n := countOf(m.calls(), "subscribe t1"); n != 3 {
			t.Errorf("%d subscribes, want 3, one per reconnect", n)
		}
	})
}

func countOf(calls []string, name string) int {
	return len(slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return c != name }))
}

func TestOutcomeIsUnknownWhenTheStreamIsLostBeforeATaskIsNamed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := make(chan item)
		m := newMemConn(t, []chan item{send}, nil)
		s := sendAsync(t, memAgent(t, m, nil))
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		err := s.result()
		if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrStreamLost) || errors.Is(err, ErrNotSent) {
			t.Errorf("err = %v, want ErrOutcomeUnknown only", err)
		}
		if got, want := m.calls(), []string{"send", "send ended"}; !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q: there is no task to ask about", got, want)
		}
	})
}

// A Source that fails while reconnecting must not lose the task the agent named.
func TestSourceFailureDuringReconnectKeepsTheTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, boom := make(chan item), errors.New("secret not found")
		m := newMemConn(t, []chan item{send}, nil)
		var calls int
		source := func(context.Context) (Conn, error) {
			if calls++; calls > 1 {
				return nil, boom
			}
			return m, nil
		}
		s := sendAsync(t, New(source, Config{ReconnectDelay: time.Second}))
		s.feed(send, item{ev: working()})
		s.feed(send, item{err: io.ErrUnexpectedEOF})
		err := s.result()
		var lost *StreamLostError
		if !errors.Is(err, ErrStreamLost) || !errors.Is(err, boom) || !errors.As(err, &lost) || lost.TaskID != "t1" || lost.ContextID != "c1" || errors.Is(err, ErrNotSent) {
			t.Errorf("err = %v, want a StreamLostError for t1 in c1 wrapping the Source's error", err)
		}
	})
}

// A Source that fails before anything is sent is its own outcome: retrying is safe.
func TestSourceFailureBeforeSendingIsNotSent(t *testing.T) {
	boom := errors.New("secret not found")
	err := New(func(context.Context) (Conn, error) { return nil, boom }, Config{}).Send(t.Context(), message("hi"), nil)
	if !errors.Is(err, ErrNotSent) || !errors.Is(err, boom) || errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrStreamLost) {
		t.Errorf("err = %v, want ErrNotSent wrapping the Source's error", err)
	}
}
