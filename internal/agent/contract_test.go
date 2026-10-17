package agent

import (
	"net/http"
	"slices"
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

// agentMessage is a JSON-RPC result that is a single agent Message, with no task.
const agentMessage = `{"jsonrpc":"2.0","id":"1","result":{"message":{"messageId":"m-reply","contextId":"ctx-1","role":"ROLE_AGENT","parts":[{"text":"the answer, "},{"text":"in one message"}]}}}`

// A2A lets an agent answer a send with one Message and no task, on the stream or, for an agent whose
// card does not stream, from the unary SendMessage a2a-go falls back to. Its text is the reply, and the
// turn ends with it: a COMPLETED final event that names no task but keeps the contextId (spec 8.3).
func TestMessageReplyIsTheOutput(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noStreaming bool
		method      string
		serve       func(http.ResponseWriter)
	}{
		{"streamed", false, "SendStreamingMessage", func(w http.ResponseWriter) { write(w, []byte("data: "+agentMessage+"\n\n")) }},
		{"unary", true, "SendMessage", func(w http.ResponseWriter) { _, _ = w.Write([]byte(agentMessage)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { tc.serve(w) })
			fa.noStreaming = tc.noStreaming
			h, got := collector()
			if err := fa.agent(t).Send(t.Context(), message("hi"), h); err != nil {
				t.Fatalf("Send = %v, want the message as the reply", err)
			}
			if fa.count(tc.method) != 1 {
				t.Errorf("%s calls = %d, want 1", tc.method, fa.count(tc.method))
			}
			if want := []string{"the answer, in one message"}; !slices.Equal(texts(*got), want) {
				t.Errorf("texts = %q, want %q", texts(*got), want)
			}
			want := []a2a.TaskState{a2a.TaskStateCompleted}
			if !slices.Equal(states(*got), want) {
				t.Errorf("states = %v, want %v", states(*got), want)
			}
			if se, ok := (*got)[0].(StateEvent); !ok || se.TaskID != "" || se.ContextID != "ctx-1" {
				t.Errorf("first event = %+v, want the state, with the context and no task, before any text", (*got)[0])
			}
			final, ok := (*got)[len(*got)-1].(FinalEvent)
			if !ok {
				t.Fatalf("last event = %#v, want a FinalEvent", (*got)[len(*got)-1])
			}
			if task := final.Task; task.ID != "" || task.ContextID != "ctx-1" || task.Status.State != a2a.TaskStateCompleted || task.Status.Message == nil || task.Status.Message.ID != "m-reply" {
				t.Errorf("final task = %+v, want a COMPLETED task with no ID, the context, and the message as its status", task)
			}
		})
	}
}

// The message's ID keys its text, as an artifact's ID would, so two replies never share one.
// A Message after the agent has named a task is not the reply: the task's own events are.
func TestAssemblerFoldsAMessageOnlyBeforeATask(t *testing.T) {
	h, got := collector()
	asm := newAssembler(h, nil)
	asm.apply(working())
	asm.apply(&a2a.Message{ID: "m1", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart("stray")}})
	if len(texts(*got)) != 0 || asm.final != nil {
		t.Errorf("events = %+v, final = %v; want a message after the task ignored", *got, asm.final)
	}
}
