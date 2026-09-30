package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
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

// R17: a wrong answer wedges the task; the caller cancels it by the task the answer named.
func TestWedgedTaskIsCancelledByItsAnswer(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "CancelTask" {
			reply(t, w, "busy/04-cancel.response.json")
			return
		}
		sse(t, w, "askuser/10-bad-empty-answers.response.sse", -1)
	})
	agent, answer := fa.agentWith(t, quick), message("")
	answer.TaskID = "task-9"
	err := agent.Answer(t.Context(), answer, nil)
	if !errors.Is(err, ErrAnswerRejected) {
		t.Fatalf("Answer err = %v, want ErrAnswerRejected", err)
	}
	if _, err := agent.CancelTask(t.Context(), answer.TaskID); err != nil {
		t.Fatal(err)
	}
	if got := string(fa.callAt(1).Params); got != `{"id":"task-9"}` {
		t.Errorf("CancelTask params = %s, want the answer's task", got)
	}
}
