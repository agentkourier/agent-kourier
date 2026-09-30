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
		m := &memConn{sends: []chan item{send}, subs: []chan item{sub1, sub2, sub3}, gets: []getResult{{task: task(a2a.TaskStateWorking, "")}}}
		result, _ := sendAsync(t, memAgent(m, func(c *Config) { c.MaxReconnects = 3 }))
		send <- item{ev: working()}
		send <- item{err: io.ErrUnexpectedEOF}
		for _, sub := range []chan item{sub1, sub2, sub3} {
			sub <- item{ev: task(a2a.TaskStateWorking, "")} // the opening snapshot, then silence
		}
		err := result()
		var lost *StreamLostError
		if !errors.Is(err, ErrStreamLost) || !errors.As(err, &lost) || lost.TaskID != "t1" || lost.ContextID != "c1" {
			t.Fatalf("err = %v, want a StreamLostError for t1 in c1", err)
		}
		if n := countOf(m.calls(), "subscribe"); n != 3 {
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
		m := &memConn{sends: []chan item{send}}
		result, _ := sendAsync(t, memAgent(m, nil))
		send <- item{err: io.ErrUnexpectedEOF}
		err := result()
		if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrStreamLost) || errors.Is(err, ErrNotSent) {
			t.Errorf("err = %v, want ErrOutcomeUnknown only", err)
		}
		if n := countOf(m.calls(), "get"); n != 0 {
			t.Errorf("%d GetTask calls, want none: there is no task to ask about", n)
		}
	})
}

// A Source that fails while reconnecting must not lose the task the agent named.
func TestSourceFailureDuringReconnectKeepsTheTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send, boom := make(chan item), errors.New("secret not found")
		m := &memConn{sends: []chan item{send}}
		var calls int
		source := func(context.Context) (Conn, error) {
			if calls++; calls > 1 {
				return nil, boom
			}
			return m, nil
		}
		result, _ := sendAsync(t, New(source, nil, Config{ReconnectDelay: time.Second}))
		send <- item{ev: working()}
		send <- item{err: io.ErrUnexpectedEOF}
		err := result()
		var lost *StreamLostError
		if !errors.Is(err, ErrStreamLost) || !errors.Is(err, boom) || !errors.As(err, &lost) || lost.TaskID != "t1" || lost.ContextID != "c1" || errors.Is(err, ErrNotSent) {
			t.Errorf("err = %v, want a StreamLostError for t1 in c1 wrapping the Source's error", err)
		}
	})
}

// A Source that fails before anything is sent is its own outcome: retrying is safe.
func TestSourceFailureBeforeSendingIsNotSent(t *testing.T) {
	boom := errors.New("secret not found")
	err := New(func(context.Context) (Conn, error) { return nil, boom }, nil, Config{}).Send(t.Context(), message("hi"), nil)
	if !errors.Is(err, ErrNotSent) || !errors.Is(err, boom) || errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrStreamLost) {
		t.Errorf("err = %v, want ErrNotSent wrapping the Source's error", err)
	}
}
