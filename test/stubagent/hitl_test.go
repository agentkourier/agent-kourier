package stubagent_test

import (
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const (
	hitlURI       = stubagent.HITLExtension
	inputRequired = "TASK_STATE_INPUT_REQUIRED"
	dbQuestion    = "Which database should we use?"
)

// withHITL makes the client ask for the extension, as a2a-go's activator does.
func withHITL(c *client) *client {
	c.hdr.Set("A2A-Extensions", hitlURI)
	return c
}

// dbAsk is the ask-user turn of the spike's spike-ask agent: one question with two choices.
func dbAsk(then func(stubagent.Response) []stubagent.Step) stubagent.Step {
	return stubagent.Ask([]stubagent.Question{{Question: dbQuestion, Choices: []string{"PostgreSQL", "MySQL"}}}, then)
}

// answers is the HITL payload of an ask-user reply: one answer list per question.
func answers(requestID string, lists ...[]string) obj {
	as := []obj{}
	for _, l := range lists {
		as = append(as, obj{"answer": l})
	}
	return obj{"type": "ask_user_response", "id": requestID, "answers": as}
}

// reply builds a message to a paused task, with the HITL payload if there is one.
func reply(id, text, ctxID, taskID string, payload obj) obj {
	params := msg(id, text, ctxID)
	m := params["message"].(obj)
	m["taskId"] = taskID
	if payload != nil {
		m["extensions"] = []string{hitlURI}
		m["metadata"] = obj{hitlURI: payload}
	}
	return params
}

// paused sends a prompt and returns the stream of the pause, with the task and context ids and
// the request the agent sent, if it sent one.
func paused(t *testing.T, c *client) (evs []obj, taskID, ctxID string, request obj) {
	t.Helper()
	evs = c.send(msg("m1", "ask", ""))
	taskID, ctxID, state := ids(last(evs))
	if state != inputRequired {
		t.Fatalf("turn ended %s, want %s", state, inputRequired)
	}
	request, _ = dig(last(evs), "result", "task", "status", "message", "metadata", hitlURI).(obj)
	return evs, taskID, ctxID, request
}

func TestAskPausesLikeKagentWithTheExtension(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(dbAsk(nil))

	evs, _, ctxID, request := paused(t, withHITL(c))

	rec := fixture(t, "askuser/02-ask.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := range evs {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	want := obj{"type": "ask_user_request", "id": request["id"], "questions": []any{
		obj{"question": dbQuestion, "choices": []any{"PostgreSQL", "MySQL"}, "multiple": false},
	}}
	if !reflect.DeepEqual(request, want) || request["id"] == "" {
		t.Errorf("request = %v, want %v", request, want)
	}
	msgOf, _ := dig(last(evs), "result", "task", "status", "message").(obj)
	if !reflect.DeepEqual(msgOf["extensions"], []any{hitlURI}) || text(msgOf["parts"]) != dbQuestion || msgOf["role"] != "ROLE_AGENT" || msgOf["contextId"] != ctxID {
		t.Errorf("status message = %v, want the question as text, the extension URI and the agent role", msgOf)
	}
}

func TestAskWithoutTheExtensionHasOnlyTheQuestionText(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(dbAsk(nil))

	evs, taskID, _, request := paused(t, c)

	rec := fixture(t, "askuser/01-ask-without-extension-header.response.sse")
	expectLike(t, "paused task", last(evs), last(rec))
	if request != nil || text(dig(last(evs), "result", "task", "status", "message", "parts")) != dbQuestion {
		t.Errorf("paused task = %v, want the question as text and no HITL payload", dig(last(evs), "result", "task", "status", "message"))
	}
	// The payload is chosen per request: the same task read with the extension has it.
	_, got := withHITL(c).call("GetTask", obj{"id": taskID})
	if dig(got, "result", "status", "message", "metadata", hitlURI) == nil {
		t.Errorf("GetTask with the extension = %v, want the HITL payload", got)
	}
	c.hdr.Del("A2A-Extensions")
	_, got = c.call("GetTask", obj{"id": taskID})
	if dig(got, "result", "status", "message", "extensions") != nil || dig(got, "result", "status", "message", "metadata", hitlURI) != nil {
		t.Errorf("GetTask without the extension = %v, want no HITL payload", got)
	}
}

func TestAskWithSeveralQuestionsAndFreeText(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Ask([]stubagent.Question{
		{Question: "Which?", Choices: []string{"a", "b"}, Multiple: true},
		{Question: "Any colour?"},
	}, nil))

	evs, _, _, request := paused(t, withHITL(c))

	qs, _ := request["questions"].([]any)
	if len(qs) != 2 || dig(qs[0], "multiple") != true || dig(qs[1], "choices") != nil || dig(qs[1], "question") != "Any colour?" {
		t.Errorf("questions = %v, want a multi-select and a free-text question with no choices", qs)
	}
	if got := text(dig(last(evs), "result", "task", "status", "message", "parts")); got != "Which?\nAny colour?" {
		t.Errorf("status text = %q, want the questions joined by newlines", got)
	}
}
