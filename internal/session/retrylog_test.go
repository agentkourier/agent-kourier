package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

var (
	errBusySend     = fmt.Errorf("send message: %w", agent.ErrBusy)
	errNotReadySend = fmt.Errorf("send message: %w: %w", agent.ErrNotReady, agent.ErrBusy)
)

// A turn the agent answers -32004, busy or not ready, is sent again by design, and the wait is no failure: each
// attempt logs one line at Info, naming the reason and the attempt, and nothing at Error. The line holds neither
// the person's message nor the agent's error text.
func TestExpectedRetriesLogAtInfo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"busy", errBusySend, "busy"},
		{"not ready", errNotReadySend, "not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, logs, _ := logged(t)
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				if n < 3 {
					return tc.err
				}
				return turn(h, n, a2a.TaskStateCompleted)
			}
			in := inbound("1")
			in.Text = "my password is FAKE-PROMPT"
			if _, err := f.submit(in); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			f.retired(t)

			waits := logs.lines("session turn waiting")
			if len(waits) != 3 {
				t.Fatalf("%d waiting lines; want one per refused send, 3:\n%s", len(waits), logs.everything())
			}
			for i, rec := range waits {
				if rec.level != slog.LevelInfo {
					t.Errorf("waiting line %d is at %v; want Info", i+1, rec.level)
				}
				if rec.attrs["reason"] != tc.reason || rec.attrs["attempt"] != int64(i+1) {
					t.Errorf("waiting line %d = %v; want reason %s and attempt %d", i+1, rec.attrs, tc.reason, i+1)
				}
			}
			if got := logs.lines("session turn failed"); len(got) != 0 {
				t.Errorf("%d failed lines; want none: the wait is expected", len(got))
			}
			logs.mu.Lock()
			defer logs.mu.Unlock()
			for _, rec := range *logs.recs {
				if rec.level >= slog.LevelWarn {
					t.Errorf("a %v line %q; want none above Info for a retry the agent asked for", rec.level, rec.msg)
				}
			}
			if all := logs.everything(); strings.Contains(all, "FAKE-PROMPT") {
				t.Errorf("the log holds the person's message:\n%s", all)
			}
		})
	}
}

// A send that fails for any other reason is still a failure, logged at Error as before.
func TestRealFailureStillLogsAtError(t *testing.T) {
	f, _, logs, _ := logged(t)
	f.agent.script = func(context.Context, int, agent.Handler) error { return errors.New("the agent fell over") }
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure line", func() bool { return len(logs.lines("session turn failed")) >= 1 })
	f.retired(t) // the turn stays queued, and the runner gives up on this wake
	got := logs.lines("session turn failed")
	if len(got) != 1 || got[0].level != slog.LevelError {
		t.Fatalf("failed lines = %+v; want one, at Error", got)
	}
	if w := logs.lines("session turn waiting"); len(w) != 0 {
		t.Errorf("%d waiting lines; want none for a failure", len(w))
	}
}

// An answer the agent refuses with -32004 is not delivered, and Agent Kourier does not send it again: the thread is told
// so, in words of the reason (a session not ready is no busy task), and the task is followed, or the pause
// reopens for the person to answer again, as it does for a busy one.
func TestRefusedAnswerSaysWhy(t *testing.T) {
	inputRequired := func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &t0}}, nil
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"busy", errBusySend, "the agent is busy with the task, and did not take the answer"},
		{"not ready", errNotReadySend, "the agent's session was not ready, and did not take the answer"},
	} {
		for _, st := range []struct {
			name   string
			get    func(a2a.TaskID) (*a2a.Task, error)
			reopen bool // the pause reopens, rather than the task being followed
		}{
			{"the task runs", working(t0.Add(2 * time.Hour)), false},
			{"the task still waits", inputRequired, true},
		} {
			t.Run(tc.name+", "+st.name, func(t *testing.T) {
				f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error { return tc.err })
				f.agent.mu.Lock()
				f.agent.get = st.get
				f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
				f.agent.mu.Unlock()
				f.now.Store(t0.Add(2 * time.Hour).UnixNano())
				if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
					t.Fatal(err)
				}
				eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
				if got := sink.noticesOf(NoticeFailed)[0]; got.Detail != tc.want {
					t.Errorf("detail = %q; want %q", got.Detail, tc.want)
				}
				if st.reopen {
					eventually(t, "the pause to reopen", func() bool { return f.m.Answer(t.Context(), key, answerMsg("ans-2")) == nil })
				} else if id, want := f.nextSent(), inbound("2").MessageID; id != want {
					t.Errorf("sent %s; want the held reply %s: the task is followed to its end", id, want)
				}
			})
		}
	}
}
