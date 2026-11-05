package agent

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

const wireSettled = `data: {"jsonrpc":"2.0","id":"1","result":{"statusUpdate":{"taskId":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}}` + "\n\n"

// oversizedAnswer is a JSON-RPC answer of one byte more than dialect.MaxResponseBytes, as the answer to GetTask, CancelTask
// and ListTasks of a task whose history has grown past what Agent Kourier reads.
func oversizedAnswer() []byte {
	const head = `{"jsonrpc":"2.0","id":"1","result":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}`
	return []byte(head + strings.Repeat(" ", dialect.MaxResponseBytes+1-len(head)))
}

// Every unary call of the agent package is bounded at dialect.MaxResponseBytes, and an answer past it is the error that
// ends the turn with its notice, not a lost stream: the agent would send the same answer again, so the call is made once
// and never again, and nothing waits for a pause between tries. The cases are the callers of GetTask (the call, a follow, a
// resume after a lost stream, the read after a stream that ended with the task settled and no final task), of CancelTask,
// and of ListTasks (the lookup of a turn).
func TestUnaryAnswerPastTheBoundIsTooLargeAndMadeOnce(t *testing.T) {
	big := oversizedAnswer()
	lost := func(w http.ResponseWriter) { write(w, []byte(wireWorking)); drop() }
	settledNoTask := func(w http.ResponseWriter) { write(w, []byte(wireWorking)); write(w, []byte(wireSettled)) }
	for _, tc := range []struct {
		name   string
		stream func(w http.ResponseWriter)
		call   func(a *Agent, h Handler) error
		method string // the unary method that carried the answer
	}{
		{"GetTask", nil, func(a *Agent, _ Handler) error { _, err := a.GetTask(t.Context(), "t-1"); return err }, "GetTask"},
		{"CancelTask", nil, func(a *Agent, _ Handler) error { _, err := a.CancelTask(t.Context(), "t-1"); return err }, "CancelTask"},
		{"FindTask", nil, func(a *Agent, _ Handler) error { _, _, err := a.FindTask(t.Context(), "c-1", "m-1"); return err }, "ListTasks"},
		{"Follow", nil, func(a *Agent, h Handler) error { return a.Follow(t.Context(), "t-1", h) }, "GetTask"},
		{"Send, after a lost stream", lost, func(a *Agent, h Handler) error { return a.Send(t.Context(), message("go"), h) }, "GetTask"},
		{"Send, after a stream that ended with the task settled and no final task", settledNoTask,
			func(a *Agent, h Handler) error { return a.Send(t.Context(), message("go"), h) }, "GetTask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
				switch call.Method {
				case "SendStreamingMessage":
					tc.stream(w)
				case tc.method:
					_, _ = w.Write(big)
				}
			})
			h, _ := collector()
			err := tc.call(fa.agentViaWith(t, generic.New(0), quick), h)
			if !errors.Is(err, dialect.ErrResponseTooLarge) {
				t.Fatalf("err = %v, want an error that wraps dialect.ErrResponseTooLarge", err)
			}
			if errors.Is(err, ErrStreamLost) {
				t.Errorf("err = %v: an answer that is too large is no lost stream", err)
			}
			if n := fa.count(tc.method); n != 1 {
				t.Errorf("%s called %d times, want once: the same answer comes again", tc.method, n)
			}
			if n := fa.count("SubscribeToTask"); n != 0 {
				t.Errorf("SubscribeToTask called %d times: a task that cannot be read is not followed", n)
			}
		})
	}
}

// The task a person's answer replies to is read first, to tell what is new in the reply from what the task held already,
// and a task that cannot be read is answered without it. One that is too large is such a task: the answer is sent, once,
// and its own stream is bounded as any is.
func TestAnswerGoesOnWhenThePausedTaskIsTooLargeToRead(t *testing.T) {
	big := oversizedAnswer()
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		switch call.Method {
		case "GetTask":
			_, _ = w.Write(big)
		case "SendStreamingMessage":
			write(w, []byte(wireWorking))
			write(w, []byte(wireDone))
		}
	})
	msg := message("yes")
	msg.TaskID = a2a.TaskID("t-1")
	h, got := collector()
	if err := fa.agentViaWith(t, generic.New(0), quick).Answer(t.Context(), msg, h); err != nil {
		t.Fatalf("Answer = %v, want the answer sent and the turn ended", err)
	}
	if n, s := fa.count("GetTask"), fa.count("SendStreamingMessage"); n != 1 || s != 1 {
		t.Errorf("%d GetTask and %d sends, want one each", n, s)
	}
	if last, ok := (*got)[len(*got)-1].(FinalEvent); !ok || last.Task == nil {
		t.Errorf("last event = %#v, want the final event", (*got)[len(*got)-1])
	}
}
