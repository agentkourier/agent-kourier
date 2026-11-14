package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// following is a Follow running in a goroutine of the test's synctest bubble.
type following struct {
	t    *testing.T
	done chan error
	got  *[]Event
}

func followAsync(t *testing.T, a *Agent) *following {
	t.Helper()
	h, got := collector()
	f := &following{t: t, done: make(chan error, 1), got: got}
	go func() { f.done <- a.Follow(t.Context(), "t1", h) }()
	return f
}

func (f *following) result() error {
	f.t.Helper()
	select {
	case err := <-f.done:
		return err
	case <-time.After(time.Minute):
		f.t.Fatalf("Follow never returned")
		return nil
	}
}

// A task that has ended has nothing to subscribe to (spec 8.3): the caller gets the whole
// text and the final task, with one GetTask.
func TestFollowFinishedTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemConn(t, nil, nil, getResult{task: task(a2a.TaskStateCompleted, "ab")})
		f := followAsync(t, memAgent(t, m, nil))
		if err := f.result(); err != nil {
			t.Fatal(err)
		}
		if got, want := m.calls(), []string{"get t1"}; !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q", got, want)
		}
		evs := *f.got
		final, ok := evs[len(evs)-1].(FinalEvent)
		if !ok || final.Task.Status.State != a2a.TaskStateCompleted || !slices.Equal(texts(evs), []string{"ab"}) {
			t.Errorf("events = %#v, want the text ab and the COMPLETED task", evs)
		}
	})
}

// A task that still runs is reconciled the way a dropped stream is: GetTask, then
// SubscribeToTask. The first GetTask comes at once, with no reconnect pause, and no text
// reaches the caller twice.
func TestFollowRunningTaskSubscribesWithoutRepeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sub := make(chan item)
		m := newMemConn(t, nil, []chan item{sub}, getResult{task: task(a2a.TaskStateWorking, "tick 1 ")})
		start := time.Now()
		f := followAsync(t, memAgent(t, m, nil))
		feed(t, sub, item{ev: task(a2a.TaskStateWorking, "tick 1 ")}, f.done) // the opening snapshot repeats the text
		feed(t, sub, item{ev: delta("tick 2")}, f.done)
		feed(t, sub, item{ev: task(a2a.TaskStateCompleted, "tick 1 tick 2")}, f.done)
		if err := f.result(); err != nil {
			t.Fatal(err)
		}
		if got, want := m.calls(), []string{"get t1", "subscribe t1", "subscribe ended"}; !slices.Equal(got, want) {
			t.Errorf("calls = %q, want %q", got, want)
		}
		if at, ok := m.at("get t1"); !ok || !at.Equal(start) {
			t.Errorf("GetTask came at %v, want %v: a first look has no pause", at, start)
		}
		if got, want := texts(*f.got), []string{"tick 1 ", "tick 1 tick 2"}; !slices.Equal(got, want) {
			t.Errorf("texts = %q, want %q", got, want)
		}
	})
}

// A pause is a settled state: the caller gets the task holding the question, for the
// renderer and the interaction (spec 8.4 step 1), and nothing is subscribed to.
func TestFollowPausedTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemConn(t, nil, nil, getResult{task: task(a2a.TaskStateInputRequired, "")})
		f := followAsync(t, memAgent(t, m, nil))
		if err := f.result(); err != nil {
			t.Fatal(err)
		}
		evs := *f.got
		final, ok := evs[len(evs)-1].(FinalEvent)
		if !ok || final.Task.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("events = %#v, want a FinalEvent for the INPUT_REQUIRED task", evs)
		}
		if got := m.calls(); !slices.Equal(got, []string{"get t1"}) {
			t.Errorf("calls = %q, want one GetTask", got)
		}
	})
}

// A subscription that drops is resumed like a send that drops, and running out of reconnects
// is a StreamLostError that names the task.
func TestFollowLosesTheStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemConn(t, nil, nil, getResult{err: errors.New("down")})
		f := followAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 2 }))
		err := f.result()
		var lost *StreamLostError
		if !errors.Is(err, ErrStreamLost) || !errors.As(err, &lost) || lost.TaskID != "t1" {
			t.Fatalf("err = %v, want a StreamLostError for t1", err)
		}
		if n := countOf(m.calls(), "get t1"); n != 3 { // the first look, then one per reconnect
			t.Errorf("%d GetTask calls, want 3", n)
		}
	})
}

func TestFollowSurvivesADroppedSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sub1, sub2 := make(chan item), make(chan item)
		m := newMemConn(t, nil, []chan item{sub1, sub2}, getResult{task: task(a2a.TaskStateWorking, "a")})
		f := followAsync(t, memAgent(t, m, nil))
		feed(t, sub1, item{ev: delta("b")}, f.done)
		feed(t, sub1, item{err: io.ErrUnexpectedEOF}, f.done)
		feed(t, sub2, item{ev: task(a2a.TaskStateCompleted, "abc")}, f.done)
		if err := f.result(); err != nil {
			t.Fatal(err)
		}
		if got, want := texts(*f.got), []string{"a", "ab", "abc"}; !slices.Equal(got, want) {
			t.Errorf("texts = %q, want %q", got, want)
		}
	})
}

// The errors the session manager tells apart must survive Follow: a task the agent no longer
// knows (session gone, spec 8.2), a refused identity, and a credential the front door
// refused (D34).
func TestFollowKeepsTheErrorsTheCallerActsOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		is      []error
		notLost bool // the agent answered or refused: the stream was not lost
	}{
		{"task not found", a2a.NewError(a2a.ErrTaskNotFound, "task not found"), []error{a2a.ErrTaskNotFound}, true},
		{"not the owner", a2a.NewError(a2a.ErrUnauthorized, "no"), []error{ErrForbidden}, true},
		{"401", fmt.Errorf("get: %w", &dialect.HTTPStatusError{Code: http.StatusUnauthorized}), []error{ErrCredentialsRejected}, true},
		{"403", fmt.Errorf("get: %w", &dialect.HTTPStatusError{Code: http.StatusForbidden}), []error{ErrCredentialsRejected}, true},
		{"404", fmt.Errorf("get: %w", &dialect.HTTPStatusError{Code: http.StatusNotFound}), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := newMemConn(t, nil, nil, getResult{err: tc.err})
				err := followAsync(t, memAgent(t, m, nil)).result()
				for _, want := range tc.is {
					if !errors.Is(err, want) {
						t.Errorf("err = %v, want it to match %v", err, want)
					}
				}
				var lost *StreamLostError
				if tc.notLost && (errors.Is(err, ErrStreamLost) || errors.As(err, &lost)) {
					t.Errorf("err = %v: the agent answered or refused, so this is not a lost stream", err)
				}
				var sent, got *dialect.HTTPStatusError
				if errors.As(tc.err, &sent) && (!errors.As(err, &got) || got.Code != sent.Code) {
					t.Errorf("err = %v, want the HTTP status %d kept", err, sent.Code)
				}
				if n := countOf(m.calls(), "get t1"); n != 1 {
					t.Errorf("%d GetTask calls, want 1: retrying an answer or a refusal cannot help", n)
				}
			})
		})
	}
}

// A 408, 429 or 503 is worth the whole budget, and running out of it loses the stream.
func TestFollowRetriesATransientRefusal(t *testing.T) {
	for _, code := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		synctest.Test(t, func(t *testing.T) {
			m := newMemConn(t, nil, nil, getResult{err: fmt.Errorf("get: %w", &dialect.HTTPStatusError{Code: code})})
			err := followAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 2 })).result()
			var lost *StreamLostError
			if !errors.As(err, &lost) || countOf(m.calls(), "get t1") != 3 {
				t.Errorf("%d: err = %v after %d GetTasks; want a StreamLostError after 3", code, err, countOf(m.calls(), "get t1"))
			}
		})
	}
}

// Restart-time Follow is how a wedged task (R17) is picked up again: SubscribeToTask opens
// with the WORKING snapshot and then says nothing, and the watchdog must cut it, or Follow
// never returns.
func TestFollowGivesUpOnASilentSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		subs := []chan item{make(chan item), make(chan item), make(chan item)}
		m := newMemConn(t, nil, subs, getResult{task: task(a2a.TaskStateWorking, "")})
		f := followAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 2 }))
		for _, sub := range subs {
			feed(t, sub, item{ev: task(a2a.TaskStateWorking, "")}, f.done) // the opening snapshot, then silence
		}
		err := f.result()
		var lost *StreamLostError
		if !errors.As(err, &lost) || lost.TaskID != "t1" || countOf(m.calls(), "subscribe t1") != 3 {
			t.Errorf("err = %v after %d subscribes; want a StreamLostError for t1 after 3", err, countOf(m.calls(), "subscribe t1"))
		}
	})
}

func TestFollowFailsWithoutAConnectionOrATask(t *testing.T) {
	broken := New(func(context.Context) (Conn, error) { return nil, errors.New("no token") }, Config{})
	if err := broken.Follow(t.Context(), "t1", nil); err == nil {
		t.Error("Follow with a failing Source = nil, want an error")
	}
	m := newMemConn(t, nil, nil)
	if err := memAgent(t, m, nil).Follow(t.Context(), "", nil); err == nil {
		t.Error("Follow of no task = nil, want an error")
	}
	if got := m.calls(); len(got) != 0 {
		t.Errorf("calls = %q, want none", got)
	}
}
