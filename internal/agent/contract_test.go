package agent

import (
	"errors"
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
// card does not stream, from the unary SendMessage a2a-go falls back to. Its text is the reply, keyed by
// the messageId, and the turn ends with it: a COMPLETED final event that names no task but keeps the
// contextId (spec 6.3, 8.3). There is no task to look up.
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
			if fa.count(tc.method) != 1 || fa.count("GetTask") != 0 {
				t.Errorf("%s calls = %d, GetTask calls = %d; want one send and no lookup", tc.method, fa.count(tc.method), fa.count("GetTask"))
			}
			if len(*got) != 3 {
				t.Fatalf("events = %#v, want the state, the text and the final event", *got)
			}
			if se, ok := (*got)[0].(StateEvent); !ok || se.TaskID != "" || se.ContextID != "ctx-1" || se.State != a2a.TaskStateCompleted {
				t.Errorf("first event = %#v, want the COMPLETED state, with the context and no task, before any text", (*got)[0])
			}
			if te, ok := (*got)[1].(TextEvent); !ok || te.ArtifactID != "m-reply" || te.Text != "the answer, in one message" {
				t.Errorf("second event = %#v, want the message's text keyed by its messageId", (*got)[1])
			}
			final, ok := (*got)[2].(FinalEvent)
			if !ok {
				t.Fatalf("last event = %#v, want a FinalEvent", (*got)[2])
			}
			if task := final.Task; task.ID != "" || task.ContextID != "ctx-1" || task.Status.State != a2a.TaskStateCompleted || task.Status.Message == nil || task.Status.Message.ID != "m-reply" {
				t.Errorf("final task = %+v, want a COMPLETED task with no ID, the context, and the message as its status", task)
			}
		})
	}
}

// contractTaskDone is the full COMPLETED task that ends a stream, with the answer as its artifact.
const contractTaskDone = `data: {"jsonrpc":"2.0","id":"1","result":{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"},` +
	`"artifacts":[{"artifactId":"a-1","parts":[{"text":"the answer"}]}]}}}` + "\n\n"

// streamMessage is a stream event that is a Message with the given role and taskId.
func streamMessage(role, taskID string) string {
	return `data: {"jsonrpc":"2.0","id":"1","result":{"message":{"messageId":"m-1","contextId":"c-1","taskId":"` + taskID +
		`","role":"` + role + `","parts":[{"text":"not the answer"}]}}}` + "\n\n"
}

// Only an agent's Message that names no task, in reply to a Send, is a reply of its own. Any other is
// ignored: the task's events that follow are the turn, and with none the outcome is unknown, as it was
// before Messages were read. An answer's task is known, so a Message never ends it as a task-less turn.
func TestAMessageThatIsNotAReplyIsIgnored(t *testing.T) {
	answer := func(a *Agent, t *testing.T, h Handler) error {
		msg := message("yes")
		msg.TaskID = "t-paused"
		return a.Answer(t.Context(), msg, h)
	}
	send := func(a *Agent, t *testing.T, h Handler) error { return a.Send(t.Context(), message("hi"), h) }
	for _, tc := range []struct {
		name     string
		call     func(*Agent, *testing.T, Handler) error
		events   []string
		wantText bool // the task's events that follow are the turn; else the outcome is unknown
	}{
		{"an agent Message on an answer", answer, []string{"data: " + agentMessage + "\n\n"}, false},
		{"a Message that names a task", send, []string{streamMessage("ROLE_AGENT", "t-1"), contractWorking, contractTaskDone}, true},
		{"a Message that names a task, alone", send, []string{streamMessage("ROLE_AGENT", "t-1")}, false},
		{"the user's Message echoed", send, []string{streamMessage("ROLE_USER", ""), contractWorking, contractTaskDone}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
				for _, ev := range tc.events {
					write(w, []byte(ev))
				}
			})
			h, got := collector()
			err := tc.call(fa.agentWith(t, quick), t, h)
			if !tc.wantText {
				if !errors.Is(err, ErrOutcomeUnknown) || len(*got) != 0 || fa.count("GetTask") != 0 || fa.count("CancelTask") != 0 {
					t.Errorf("err = %v, events = %#v, GetTask %d, CancelTask %d; want ErrOutcomeUnknown and nothing else",
						err, *got, fa.count("GetTask"), fa.count("CancelTask"))
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want the task's reply", err)
			}
			if all := texts(*got); !slices.Equal(all, []string{"the answer"}) {
				t.Errorf("texts = %q, want the task's answer alone", all)
			}
			if final, ok := (*got)[len(*got)-1].(FinalEvent); !ok || final.Task.ID != "t-1" {
				t.Errorf("last event = %#v, want task t-1's final event", (*got)[len(*got)-1])
			}
		})
	}
}

// A Message after the agent has named a task is not the reply: the task's own events are.
func TestAssemblerIgnoresAMessageAfterATask(t *testing.T) {
	h, got := collector()
	asm := newAssembler(h, nil)
	asm.replies = true
	asm.apply(working())
	before := len(*got)
	asm.apply(&a2a.Message{ID: "m1", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart("stray")}})
	if len(*got) != before || asm.final != nil {
		t.Errorf("events = %+v, final = %v; want a message after the task ignored", *got, asm.final)
	}
}
