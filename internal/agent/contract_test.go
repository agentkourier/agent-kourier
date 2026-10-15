package agent

import (
	"errors"
	"net/http"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The tests in this file pin reply shapes that the recorded kagent fixtures never show, because
// an agent other than kagent may send them. The published agent contract describes each one.

const (
	contractTask    = `"taskId":"t-1","contextId":"c-1"`
	contractWorking = `data: {"jsonrpc":"2.0","id":"1","result":{"statusUpdate":{` + contractTask + `,"status":{"state":"TASK_STATE_WORKING"}}}}` + "\n\n"
	contractText    = `data: {"jsonrpc":"2.0","id":"1","result":{"artifactUpdate":{` + contractTask + `,"artifact":{"artifactId":"a-1","parts":[{"text":"the answer"}]}}}}` + "\n\n"
	contractDone    = `data: {"jsonrpc":"2.0","id":"1","result":{"statusUpdate":{` + contractTask + `,"status":{"state":"TASK_STATE_COMPLETED"}}}}` + "\n\n"
	contractTaskGot = `{"jsonrpc":"2.0","id":"1","result":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"},` +
		`"artifacts":[{"artifactId":"a-1","parts":[{"text":"the answer"}]}]}}`
)

// A stream that ends after a COMPLETED status update, with no task snapshot, has not settled the
// turn: Agent reads it as a drop and settles the turn from GetTask, without subscribing again.
func TestAStreamEndingInAStatusUpdateIsSettledByGetTask(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		switch call.Method {
		case "SendStreamingMessage":
			for _, ev := range []string{contractWorking, contractText, contractDone} {
				write(w, []byte(ev))
			}
		case "GetTask":
			_, _ = w.Write([]byte(contractTaskGot))
		default:
			t.Errorf("unexpected call %s", call.Method)
		}
	})
	h, got := collector()
	if err := fa.agentWith(t, quick).Send(t.Context(), message("hi"), h); err != nil {
		t.Fatal(err)
	}
	if all := texts(*got); len(all) != 1 || all[0] != "the answer" {
		t.Errorf("texts = %q, want the answer once", all)
	}
	if final, ok := (*got)[len(*got)-1].(FinalEvent); !ok || final.Task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("last event = %#v, want the COMPLETED task", (*got)[len(*got)-1])
	}
	if n := fa.count("GetTask"); n != 1 {
		t.Errorf("GetTask called %d times, want once", n)
	}
}

// Only artifact text is the answer. Text in the status message of a COMPLETED task is not
// delivered, so a turn that answers only there shows no output.
func TestStatusMessageTextIsNotTheAnswer(t *testing.T) {
	const done = `data: {"jsonrpc":"2.0","id":"1","result":{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED",` +
		`"message":{"messageId":"m-1","role":"ROLE_AGENT","parts":[{"text":"the answer"}]}}}}}` + "\n\n"
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { write(w, []byte(done)) })
	h, got := collector()
	if err := fa.agent(t).Send(t.Context(), message("hi"), h); err != nil {
		t.Fatal(err)
	}
	if all := texts(*got); len(all) != 0 {
		t.Errorf("texts = %q, want none", all)
	}
	if _, ok := (*got)[len(*got)-1].(FinalEvent); !ok {
		t.Errorf("last event = %#v, want the final task", (*got)[len(*got)-1])
	}
}

// A reply that is a bare Message names no task, so Agent cannot tell whether the turn ran: the
// send ends with ErrOutcomeUnknown, and nothing is delivered or looked up.
func TestABareMessageReplyLeavesTheOutcomeUnknown(t *testing.T) {
	const reply = `data: {"jsonrpc":"2.0","id":"1","result":{"message":{"messageId":"m-1","contextId":"c-1","role":"ROLE_AGENT","parts":[{"text":"the answer"}]}}}` + "\n\n"
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { write(w, []byte(reply)) })
	h, got := collector()
	err := fa.agentWith(t, quick).Send(t.Context(), message("hi"), h)
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrNotSent) {
		t.Errorf("err = %v, want ErrOutcomeUnknown", err)
	}
	if len(*got) != 0 {
		t.Errorf("events = %#v, want none", *got)
	}
	if n := fa.count("GetTask"); n != 0 {
		t.Errorf("GetTask called %d times, want none", n)
	}
}
