package stubagent_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const missingAnswers = "ask_user decision is missing approval correlation or answers"

// expectRoles checks the roles of a task's history, in order.
func expectRoles(t *testing.T, ev obj, want ...string) {
	t.Helper()
	_, e := body(ev)
	hist, _ := e["history"].([]any)
	var got []string
	for _, m := range hist {
		role, _ := dig(m, "role").(string)
		got = append(got, role)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("history roles = %v, want %v (the prompt, the agent's request, the reply)", got, want)
	}
}

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
	if id != taskID || ctx != ctxID || state != completed || taskText(last(evs)) != "Using PostgreSQL for storage." {
		t.Errorf("final task = %s in %s, %s, text %q; want the same task, COMPLETED and the text", id, ctx, state, taskText(last(evs)))
	}
	// askuser/05: the request the agent sent is in the history between the prompt and the reply.
	expectRoles(t, last(evs), "ROLE_USER", "ROLE_AGENT", "ROLE_USER")
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
	expectRoles(t, last(evs), "ROLE_USER", "ROLE_AGENT", "ROLE_USER") // askuser/08
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
		stream  string    // the recorded bad answer
		get     string    // the recorded GetTask after it
		busy    [3]string // the recorded good answer, plain text and new turn that follow
	}{
		{"empty answer list", func(id string) obj { return answers(id) },
			"askuser/10-bad-empty-answers.response.sse", "askuser/11-gettask-after-bad-empty-answers.response.json",
			[3]string{"askuser/27-wedge-empty-then-good-answer.response.sse", "askuser/28-wedge-empty-then-plain-text.response.sse", "askuser/29-wedge-empty-then-new-turn.response.sse"}},
		{"unknown request id", func(string) obj { return answers("nope", []string{"PostgreSQL"}) },
			"askuser/13-bad-wrong-id.response.sse", "askuser/14-gettask-after-bad-wrong-id.response.json",
			[3]string{"askuser/35-wedge-wrong-id-then-good-answer.response.sse", "askuser/36-wedge-wrong-id-then-plain-text.response.sse", "askuser/37-wedge-wrong-id-then-new-turn.response.sse"}},
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
			rec := fixture(t, tc.stream)
			if got, want := shape(evs), "statusUpdate:WORKING, error:-32603"; got != want || st.status != 200 || !st.sse {
				t.Fatalf("bad answer = HTTP %d, sse %v, %s; want 200, true, %s", st.status, st.sse, got, want)
			}
			expectLike(t, "WORKING update", evs[0], rec[0])
			expectLike(t, "error", evs[1], rec[1])
			if dig(evs[1], "error", "message") != missingAnswers || dig(evs[1], "error", "data").([]any)[0].(obj)["reason"] != "INTERNAL_ERROR" {
				t.Errorf("error = %v, want %q with reason INTERNAL_ERROR", evs[1]["error"], missingAnswers)
			}

			// The task is stuck WORKING, with the whole conversation in its history.
			_, got := c.call("GetTask", obj{"id": taskID})
			expectLike(t, "GetTask after the bad answer", got, fixture(t, tc.get)[0])
			if _, _, state := ids(got); state != working || dig(got, "result", "status", "message") != nil {
				t.Errorf("GetTask after the bad answer: %s with message %v, want %s and none", state, dig(got, "result", "status", "message"), working)
			}
			expectRoles(t, got, "ROLE_USER", "ROLE_AGENT", "ROLE_USER")

			// Nothing gets in, and only CancelTask gets out.
			for i, params := range []obj{
				reply("m3", "x", ctxID, taskID, answers(request["id"].(string), []string{"PostgreSQL"})), // a good answer
				reply("m4", "x", ctxID, taskID, nil),                                                     // plain text
				msg("m5", "x", ctxID),                                                                    // a new turn, no task
			} {
				st := c.open("SendStreamingMessage", params)
				evs := st.all()
				info, _ := dig(evs[0], "error", "data").([]any)
				if !st.sse || len(evs) != 1 || dig(evs[0], "error", "code") != float64(-32004) || dig(evs[0], "error", "message") != "resource conflict" ||
					len(info) != 1 || dig(info[0], "reason") != "UNSUPPORTED_OPERATION" {
					t.Errorf("send %d to the wedged task = sse %v, %v; want one -32004 resource conflict event", i, st.sse, evs)
					continue
				}
				expectLike(t, "busy answer", evs[0], fixture(t, tc.busy[i])[0])
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

func TestCancelledPausedTaskTakesNoAnswer(t *testing.T) {
	_, agent, c := newAgent(t)
	called := false
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step { called = true; return nil }))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)

	c.call("CancelTask", obj{"id": taskID})
	_, late := c.call("SendStreamingMessage", reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"PostgreSQL"})))
	_, got := c.call("GetTask", obj{"id": taskID})

	// UNCONFIRMED: the code kagent answers a reply to a cancelled task with; the stub says -32004.
	if dig(late, "error", "code") != float64(-32004) || called {
		t.Errorf("answer to a cancelled task = %v, script called %v; want an error and the script not called", late, called)
	}
	if describe(got) != "task:CANCELED" {
		t.Errorf("GetTask after the refused answer = %s, want task:CANCELED", describe(got))
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

// TestReplayOfAnAnswerIsRefused: answers carry a contextId, and WP-03 measured that a replay
// carrying one is not deduplicated (oidc-replay, spec 8.2), so a replayed answer is refused,
// not joined: -32004 while the task works, and the not-waiting -32004 once it has ended. A
// blind resend of a tool approval must never run the tool twice.
// UNCONFIRMED: "replay of an answer" is on the measurement backlog.
func TestReplayOfAnAnswerIsRefused(t *testing.T) {
	s, agent, c := newAgent(t)
	calls := 0
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step {
		calls++
		return []stubagent.Step{stubagent.Text("a"), stubagent.Hold("go"), stubagent.Text("b")}
	}))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	answer := reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"}))
	st := c.open("SendStreamingMessage", answer)
	for range 4 { // WORKING, the echo, WORKING, the answer's function_response
		if _, ok := st.next(); !ok {
			t.Fatal("the answer's stream ended early")
		}
	}
	_ = st.resp.Body.Close() // the turn runs on, held at the gate

	running := c.first("SendStreamingMessage", answer)
	s.Release("go")
	if err := s.WaitTurn(t.Context(), taskID); err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}
	ended := c.first("SendStreamingMessage", answer)

	for label, got := range map[string]obj{"while the task works": running, "after it ended": ended} {
		if dig(got, "error", "code") != float64(-32004) {
			t.Errorf("replay of the answer %s = %v, want -32004", label, got)
		}
	}
	if calls != 1 {
		t.Errorf("the script ran %d times, want once", calls)
	}
}

// TestAnswerWaitsForABusySession: another task of the session is working, so its one-task
// limit refuses the answer, as it would a new message; the same answer is taken once the other
// task has ended. A paused task does not itself make the session busy.
func TestAnswerWaitsForABusySession(t *testing.T) {
	s, agent, c := newAgent(t)
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step { return []stubagent.Step{stubagent.Text("answered")} }))
	agent.Next(stubagent.Hold("sibling"))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	sibling := c.open("SendStreamingMessage", msg("m2", "another turn", ctxID)) // a new task in the session, held
	var siblingID string
	for range 3 { // SUBMITTED twice, WORKING
		ev, _ := sibling.next()
		siblingID, _, _ = ids(ev)
	}
	answer := reply("m3", "x", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"}))

	_, refused := c.call("SendStreamingMessage", answer)
	s.Release("sibling")
	if err := s.WaitTurn(t.Context(), siblingID); err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}
	evs := c.send(answer)

	if dig(refused, "error", "code") != float64(-32004) || dig(refused, "error", "message") != "resource conflict" {
		t.Errorf("answer while another task works = %v, want -32004 resource conflict", refused)
	}
	if describe(last(evs)) != "task:COMPLETED" || taskText(last(evs)) != "answered" {
		t.Errorf("the same answer after the other task ended = %s, want COMPLETED", describe(last(evs)))
	}
}

// TestThenRunsOutsideTheLock: then may call the Server. Two answers can both be inside it; the
// first to finish takes the task, and the other, finding it working, is refused.
func TestThenRunsOutsideTheLock(t *testing.T) {
	s, agent, c := newAgent(t)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step {
		s.Release("from-then") // takes the server's lock
		entered <- struct{}{}
		<-release
		return nil
	}))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	results := make(chan obj, 2)
	for _, id := range []string{"m2", "m3"} {
		go func() {
			evs := c.open("SendStreamingMessage", reply(id, "x", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"}))).all()
			results <- last(evs)
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the second answer never reached then: then ran under the server's lock")
		}
	}
	close(release)

	a, b := <-results, <-results

	refused := 0
	for _, got := range []obj{a, b} {
		if dig(got, "error", "code") == float64(-32004) {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("of two simultaneous answers %d was refused, want exactly one: %v and %v", refused, a, b)
	}
}
