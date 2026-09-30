package stubagent_test

import (
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const missingAnswers = "ask_user decision is missing approval correlation or answers"

func TestAnswerResumesTheTaskLikeKagent(t *testing.T) {
	_, agent, c := newAgent(t)
	var seen stubagent.Response
	agent.Next(dbAsk(func(r stubagent.Response) []stubagent.Step {
		seen = r
		return []stubagent.Step{stubagent.Text("Using " + r.Answers[0][0] + " for storage.")}
	}))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)

	evs := c.send(reply("m2", "PostgreSQL", ctxID, taskID, answers(request["id"].(string), []string{"PostgreSQL"})))

	rec := fixture(t, "askuser/04-good-answer.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := range evs {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	if !reflect.DeepEqual(seen.Answers, [][]string{{"PostgreSQL"}}) || seen.Plain() || seen.Text != "PostgreSQL" {
		t.Errorf("the script saw %+v, want the answer PostgreSQL and the message text", seen)
	}
	id, ctx, state := ids(last(evs))
	hist, _ := dig(last(evs), "result", "task", "history").([]any)
	if id != taskID || ctx != ctxID || state != completed || taskText(last(evs)) != "Using PostgreSQL for storage." || len(hist) != 2 {
		t.Errorf("final task = %s in %s, %s, text %q, %d history messages; want the same task, COMPLETED, the text, 2 messages",
			id, ctx, state, taskText(last(evs)), len(hist))
	}
	if result := dig(firstPart(evs[3]), "data", "response", "result"); result != `[{"answer":["PostgreSQL"],"question":"`+dbQuestion+`"}]` {
		t.Errorf("the answer's function_response = %v, want the answered questions", result)
	}
}

func TestPlainReplyIsANewUserTurn(t *testing.T) {
	_, agent, c := newAgent(t)
	var seen stubagent.Response
	agent.Next(dbAsk(func(r stubagent.Response) []stubagent.Step {
		seen = r
		return []stubagent.Step{stubagent.Text("Saw a plain user message.")}
	}))
	c = withHITL(c)
	_, taskID, ctxID, _ := paused(t, c)

	evs := c.send(reply("m2", "PostgreSQL", ctxID, taskID, nil))

	// kagent does not echo a plain reply, and has no answer artifact for it.
	rec := fixture(t, "askuser/07-bad-plain-text.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := range evs {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	if !seen.Plain() || seen.Text != "PostgreSQL" {
		t.Errorf("the script saw %+v, want a plain response with the message text", seen)
	}
}

func TestAnswersAreNotValidatedAgainstTheQuestions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lists  [][]string
		result string // the function_response artifact that records the answers
	}{
		{"not a choice", [][]string{{"Oracle"}}, `[{"answer":["Oracle"],"question":"Which?"},{"answer":[],"question":"Colour?"}]`},
		{"one answer for two questions", [][]string{{"PostgreSQL"}}, `[{"answer":["PostgreSQL"],"question":"Which?"},{"answer":[],"question":"Colour?"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, agent, c := newAgent(t)
			var seen stubagent.Response
			agent.Next(stubagent.Ask([]stubagent.Question{
				{Question: "Which?", Choices: []string{"PostgreSQL", "MySQL"}}, {Question: "Colour?"},
			}, func(r stubagent.Response) []stubagent.Step { seen = r; return nil }))
			c = withHITL(c)
			_, taskID, ctxID, request := paused(t, c)

			evs := c.send(reply("m2", "x", ctxID, taskID, answers(request["id"].(string), tc.lists...)))

			if describe(last(evs)) != "task:COMPLETED" || !reflect.DeepEqual(seen.Answers, tc.lists) {
				t.Errorf("ended %s, the script saw %v; want COMPLETED with %v as sent", describe(last(evs)), seen.Answers, tc.lists)
			}
			if result := dig(firstPart(evs[3]), "data", "response", "result"); result != tc.result {
				t.Errorf("answer artifact = %v, want %s", result, tc.result)
			}
		})
	}
}

func TestBadAnswerWedgesTheTaskUntilCancelled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload func(requestID string) obj
	}{
		{"empty answer list", func(id string) obj { return answers(id) }},
		{"unknown request id", func(string) obj { return answers("nope", []string{"PostgreSQL"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, agent, c := newAgent(t)
			called := false
			agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step { called = true; return nil }))
			agent.Next(stubagent.Text("a new turn"))
			c = withHITL(c)
			_, taskID, ctxID, request := paused(t, c)

			st := c.open("SendStreamingMessage", reply("m2", "x", ctxID, taskID, tc.payload(request["id"].(string))))
			evs := st.all()

			// The error arrives as an SSE event after a WORKING update, over HTTP 200.
			rec := fixture(t, "askuser/10-bad-empty-answers.response.sse")
			if got, want := shape(evs), "statusUpdate:WORKING, error:-32603"; got != want || st.status != 200 || !st.sse {
				t.Fatalf("bad answer = HTTP %d, sse %v, %s; want 200, true, %s", st.status, st.sse, got, want)
			}
			expectLike(t, "WORKING update", evs[0], rec[0])
			expectLike(t, "error", evs[1], rec[1])
			if dig(evs[1], "error", "message") != missingAnswers || dig(evs[1], "error", "data").([]any)[0].(obj)["reason"] != "INTERNAL_ERROR" {
				t.Errorf("error = %v, want %q with reason INTERNAL_ERROR", evs[1]["error"], missingAnswers)
			}

			// The task is stuck WORKING: nothing gets in, and only CancelTask gets out.
			_, got := c.call("GetTask", obj{"id": taskID})
			if _, _, state := ids(got); state != working {
				t.Errorf("GetTask after the bad answer: %s, want %s", state, working)
			}
			for label, params := range map[string]obj{
				"a good answer":        reply("m3", "x", ctxID, taskID, answers(request["id"].(string), []string{"PostgreSQL"})),
				"plain text":           reply("m4", "x", ctxID, taskID, nil),
				"a new turn (no task)": msg("m5", "x", ctxID),
			} {
				if _, got := c.call("SendStreamingMessage", params); dig(got, "error", "code") != float64(-32004) {
					t.Errorf("%s to the wedged task = %v, want -32004", label, got)
				}
			}
			if _, got := c.call("CancelTask", obj{"id": taskID}); describe(got) != "task:CANCELED" {
				t.Errorf("CancelTask = %v, want the CANCELED task", got)
			}
			if _, got := c.call("GetTask", obj{"id": taskID}); describe(got) != "task:CANCELED" || called {
				t.Errorf("GetTask after cancel = %s, script called %v; want CANCELED and the script never called", describe(got), called)
			}
		})
	}
}

func TestPausedTaskCanBeCancelled(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(dbAsk(nil))
	_, taskID, _, _ := paused(t, c)

	_, got := c.call("CancelTask", obj{"id": taskID})

	if describe(got) != "task:CANCELED" {
		t.Errorf("CancelTask on a paused task = %v, want the CANCELED task", got)
	}
}

func TestReplyToTasksThatCannotTakeOne(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()
	_, sent := c.call("SendMessage", msg("m1", "hi", ""))
	done := task(t, sent)

	_, unknown := c.call("SendMessage", reply("m2", "x", "", "nope", nil))
	_, finished := c.call("SendMessage", reply("m3", "x", done["contextId"].(string), done["id"].(string), nil))

	if dig(unknown, "error", "code") != float64(-32001) || dig(finished, "error", "code") != float64(-32004) {
		t.Errorf("reply to an unknown task = %v, to a completed one = %v; want -32001 and -32004", unknown, finished)
	}
}

func TestReplayOfAnAnswerRunsNoSecondTurn(t *testing.T) {
	_, agent, c := newAgent(t)
	calls := 0
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step { calls++; return []stubagent.Step{stubagent.Text("done")} }))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	answer := reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"}))
	c.send(answer)

	replay := c.send(answer)

	if got, want := shape(replay), "task:COMPLETED"; got != want || calls != 1 {
		t.Errorf("replay of the answer: %s, script called %d times; want %s and once", got, calls, want)
	}
}
