package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestUnaryCallsHaveADeadline(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, _ http.ResponseWriter) { <-call.Done }) // never answers
	cfg := quick
	cfg.UnaryTimeout = 50 * time.Millisecond
	agent := fa.agentWith(t, cfg)
	for name, call := range map[string]func() error{
		"GetTask":    func() error { _, err := agent.GetTask(t.Context(), "t1"); return err },
		"CancelTask": func() error { _, err := agent.CancelTask(t.Context(), "t1"); return err },
	} {
		start := time.Now()
		if err := call(); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Errorf("%s: err = %v after %v, want DeadlineExceeded after about 50ms", name, err, time.Since(start))
		}
	}
}

func TestGetTaskAndCancelTaskResults(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "CancelTask" {
			reply(t, w, "busy/04-cancel.response.json")
			return
		}
		reply(t, w, "identity/07-gettask-other-email-other-sub.response.json")
	})
	agent := fa.agentWith(t, quick)
	if task, err := agent.CancelTask(t.Context(), "t1"); err != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("CancelTask = %v, %v; want a CANCELED task", task, err)
	}
	if _, err := agent.GetTask(t.Context(), "t1"); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask err = %v, want ErrForbidden", err)
	}
}

// The deadline starts before the connection is resolved: a Source that waits for the
// network is bounded too.
func TestUnaryDeadlineCoversTheSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stuck := func(ctx context.Context) (Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
		agent := New(stuck, Config{UnaryTimeout: time.Second})
		if _, err := agent.GetTask(t.Context(), "t1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("GetTask err = %v, want DeadlineExceeded", err)
		}
		if _, err := agent.CancelTask(t.Context(), "t1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("CancelTask err = %v, want DeadlineExceeded", err)
		}
	})
}

// wedgeThenCancel answers every send with kagent's wedge (-32603 after WORKING) and every
// CancelTask with cancelReply.
func wedgeThenCancel(t *testing.T, cancelReply func(http.ResponseWriter)) func(rpc, http.ResponseWriter) {
	return func(call rpc, w http.ResponseWriter) {
		if call.Method == "CancelTask" {
			cancelReply(w)
			return
		}
		sse(t, w, "askuser/10-bad-empty-answers.response.sse", -1)
	}
}

// R17: an answer the agent rejects wedges the task, so Answer cancels it: the task the
// stream named, else the one the answer was addressed to.
func TestRejectedAnswerCancelsTheWedgedTask(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serve  func(rpc, http.ResponseWriter)
		wantID string
	}{
		{"the stream named the task", wedgeThenCancel(t, func(w http.ResponseWriter) { reply(t, w, "busy/04-cancel.response.json") }), "01a0f05a-a744-7b36-89fe-a087f77ac977"},
		{"the stream named no task", func(call rpc, w http.ResponseWriter) {
			if call.Method == "CancelTask" {
				reply(t, w, "busy/04-cancel.response.json")
				return
			}
			rpcError(-32603)(w)
		}, "task-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, tc.serve)
			answer := message("")
			answer.TaskID = "task-9"
			if err := fa.agentWith(t, quick).Answer(t.Context(), answer, nil); !errors.Is(err, ErrAnswerRejected) {
				t.Fatalf("Answer err = %v, want ErrAnswerRejected", err)
			}
			if fa.count("CancelTask") != 1 {
				t.Fatalf("CancelTask called %d times, want once", fa.count("CancelTask"))
			}
			if got, want := string(fa.callAt(2).Params), `{"id":"`+tc.wantID+`"}`; got != want { // call 0 reads the paused task, call 1 is the send
				t.Errorf("CancelTask params = %s, want %s", got, want)
			}
		})
	}
}

func TestRejectedAnswerReportsAFailedCancel(t *testing.T) {
	fa := newFakeAgent(t, wedgeThenCancel(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","error":{"code":-32002,"message":"not cancelable"}}`))
	}))
	err := fa.agentWith(t, quick).Answer(t.Context(), message(""), nil)
	if !errors.Is(err, ErrAnswerRejected) || !errors.Is(err, a2a.ErrTaskNotCancelable) {
		t.Errorf("err = %v, want ErrAnswerRejected joined with the failed cancel", err)
	}
}

// With no task named by the stream or by the answer, there is nothing to cancel.
func TestRejectedAnswerWithNoTaskCancelsNothing(t *testing.T) {
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { rpcError(-32603)(w) })
	err := fa.agentWith(t, quick).Answer(t.Context(), message(""), nil) // message() sets no TaskID
	if !errors.Is(err, ErrAnswerRejected) || fa.count("CancelTask") != 0 {
		t.Errorf("err = %v, CancelTask calls = %d; want ErrAnswerRejected and no cancel", err, fa.count("CancelTask"))
	}
}

// Only an answer wedges a task: a plain send that fails with -32603 cancels nothing.
func TestRejectedSendDoesNotCancel(t *testing.T) {
	fa := newFakeAgent(t, wedgeThenCancel(t, func(w http.ResponseWriter) { reply(t, w, "busy/04-cancel.response.json") }))
	if err := fa.agentWith(t, quick).Send(t.Context(), message(""), nil); errors.Is(err, ErrAnswerRejected) || fa.count("CancelTask") != 0 {
		t.Errorf("err = %v, CancelTask calls = %d; want a plain error and no cancel", err, fa.count("CancelTask"))
	}
}

// The cancel must outlive the caller's context: it is the cleanup of the failed answer.
func TestCancelOfWedgedTaskIgnoresTheCallersCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		send := make(chan item)
		m := newMemConn(t, []chan item{send}, nil)
		m.onCancel = stop
		agent := memAgent(t, m, nil)
		done := make(chan error, 1)
		go func() { done <- agent.Answer(ctx, message(""), nil) }()
		feed(t, send, item{ev: working()}, done)
		feed(t, send, item{err: a2a.NewError(a2a.ErrInternalError, "wedged")}, done)
		var err error
		select {
		case err = <-done:
		case <-time.After(time.Minute):
			t.Fatalf("Answer never returned")
		}
		if !errors.Is(err, ErrAnswerRejected) || errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want ErrAnswerRejected alone: the cancel ran on a context detached from the caller's", err)
		}
		if got := m.calls(); got[len(got)-1] != "cancel t1" {
			t.Errorf("calls = %q, want the cancel last", got)
		}
	})
}

// Answer's automatic cancel can fail, and then the wedge persists: every send to the session
// gets -32004. The caller must be able to tell that from a cancel that worked, so the failure is
// typed, joined to the rejection, and keeps the cancel's own error.
func TestFailedAutoCancelOfAWedgedTaskIsTyped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := make(chan item)
		m := newMemConn(t, []chan item{send}, nil)
		m.cancelErr = errors.New("agent unreachable")
		done := make(chan error, 1)
		go func() { done <- memAgent(t, m, nil).Answer(t.Context(), message(""), nil) }()
		feed(t, send, item{ev: working()}, done)
		feed(t, send, item{err: a2a.NewError(a2a.ErrInternalError, "wedged")}, done)
		err := <-done
		if !errors.Is(err, ErrAnswerRejected) || !errors.Is(err, ErrCancelFailed) {
			t.Errorf("err = %v; want ErrAnswerRejected and ErrCancelFailed", err)
		}
	})
}
