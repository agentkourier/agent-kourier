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
	// UNCONFIRMED: the pause is stored as it was made, so a reader that names the extension does
	// not get a payload that was never generated.
	_, got := withHITL(c).call("GetTask", obj{"id": taskID})
	if dig(got, "result", "status", "message", "extensions") != nil || dig(got, "result", "status", "message", "metadata", hitlURI) != nil {
		t.Errorf("GetTask with the extension = %v, want no HITL payload", got)
	}
}

// TestAPauseIsReadAsItWasMade: UNCONFIRMED for GetTask, SubscribeToTask and a replay
// (measured for ListTasks, oidc-identity/16): the pause made for a client that named the
// extension keeps its payload for a reader that does not.
func TestAPauseIsReadAsItWasMade(t *testing.T) {
	for _, method := range []string{"GetTask", "SubscribeToTask", "replay"} {
		t.Run(method, func(t *testing.T) {
			_, agent, c := newAgent(t)
			agent.Next(dbAsk(nil))
			_, taskID, _, _ := paused(t, withHITL(c))
			c.hdr.Del("A2A-Extensions")

			var got obj
			switch method {
			case "replay":
				got = c.first("SendStreamingMessage", msg("m1", "ask", ""))
			default:
				got = c.first(method, obj{"id": taskID})
			}

			_, e := body(got)
			if dig(e, "status", "message", "metadata", hitlURI) == nil || dig(e, "status", "message", "extensions") == nil {
				t.Errorf("%s without the extension = %v, want the pause as made, with its payload", method, got)
			}
		})
	}
}

// TestSeveralQuestionsLikeKagent reproduces askuser/21: the status text joins the questions
// with "; ", the request carries choices for every question ([] for free text) and multiple,
// and the ask_user call's args carry choices only when there are some and multiple only when it
// is true.
func TestSeveralQuestionsLikeKagent(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Ask([]stubagent.Question{
		{Question: "Which databases?", Choices: []string{"PostgreSQL", "MySQL"}, Multiple: true},
		{Question: "Favorite color?"},
	}, nil))

	evs, _, _, request := paused(t, withHITL(c))

	rec := fixture(t, "askuser/21-pause-good-two-answers.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := range evs {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	recRequest := dig(last(rec), "result", "task", "status", "message", "metadata", hitlURI).(obj)
	if !reflect.DeepEqual(request["questions"], recRequest["questions"]) {
		t.Errorf("request questions = %v, want %v", request["questions"], recRequest["questions"])
	}
	if got, want := dig(firstPart(evs[3]), "data", "args"), dig(firstPart(rec[3]), "data", "args"); !reflect.DeepEqual(got, want) {
		t.Errorf("ask_user args = %v, want %v", got, want)
	}
	if got := text(dig(last(evs), "result", "task", "status", "message", "parts")); got != "Which databases?; Favorite color?" {
		t.Errorf("status text = %q, want the questions joined by %q", got, "; ")
	}
}

// TestCancelledPausedTaskShowsNoRequest: a task that leaves INPUT_REQUIRED has no request, so
// a cancelled one has a state and no status message (askuser/30, 38, busy/04).
func TestCancelledPausedTaskShowsNoRequest(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(dbAsk(nil))
	c = withHITL(c)
	_, taskID, _, _ := paused(t, c)

	_, cancelled := c.call("CancelTask", obj{"id": taskID})
	_, got := c.call("GetTask", obj{"id": taskID})

	for label, ev := range map[string]obj{"CancelTask": cancelled, "GetTask": got} {
		if describe(ev) != "task:CANCELED" || dig(ev, "result", "status", "message") != nil {
			t.Errorf("%s on a paused task = %v, want the CANCELED task with no status message", label, ev)
		}
	}
}

// TestExtensionHeaderIsAListOfURIs: A2A-Extensions is a comma-separated list, and may be sent
// as several header lines. The extension is named only if one entry is its URI exactly.
func TestExtensionHeaderIsAListOfURIs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		named  bool
	}{
		{"alone", []string{hitlURI}, true},
		{"in a list", []string{"https://example.test/other, " + hitlURI}, true},
		{"on its own line", []string{"https://example.test/other", hitlURI}, true},
		{"with spaces around it", []string{" " + hitlURI + " "}, true},
		{"as the prefix of another URI", []string{hitlURI + "/v2"}, false},
		{"inside another URI", []string{"https://example.test/?x=" + hitlURI}, false},
		{"absent", nil, false},
	} {
		_, agent, c := newAgent(t)
		agent.Next(dbAsk(nil))
		for _, v := range tc.values {
			c.hdr.Add("A2A-Extensions", v)
		}

		_, _, _, request := paused(t, c)

		if (request != nil) != tc.named {
			t.Errorf("%s: payload present = %v, want %v", tc.name, request != nil, tc.named)
		}
	}
}
