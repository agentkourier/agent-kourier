package stubagent_test

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// notReady is a text of kagent's -32004 for a session that cannot take input, which its dialect reads as not ready.
const notReady = "cannot accept work during a lifecycle operation"

// answerAsync sends the answer to a paused task from another goroutine, and returns where its events arrive.
func answerAsync(t *testing.T, c *client, params obj) <-chan []obj {
	t.Helper()
	got := make(chan []obj, 1)
	go func() {
		evs, err := c.trySend(params)
		if err != nil {
			t.Errorf("answer: %v", err)
		}
		got <- evs
	}()
	return got
}

// expectNoAnswer fails the test if the answer arrives within a short wait: the gate it waits at is shut.
func expectNoAnswer(t *testing.T, got <-chan []obj) {
	t.Helper()
	select {
	case evs := <-got:
		t.Fatalf("the answer was answered before its gate was released: %v", evs)
	case <-time.After(50 * time.Millisecond):
	}
}

// receive waits for the events of an answer, for a while.
func receive(t *testing.T, got <-chan []obj) []obj {
	t.Helper()
	select {
	case evs := <-got:
		return evs
	case <-time.After(5 * time.Second):
		t.Fatal("no answer after the gate was released")
		return nil
	}
}

// RefuseAnswer, as the continuation of an Ask, holds the answer's send at its gate, without the server's lock,
// then refuses the answer with -32004 and its text. The task still waits for input and did not take the answer.
func TestRefuseAnswerRefusesTheAnswerOnceItsGateIsReleased(t *testing.T) {
	s, agent, c := newAgent(t)
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step {
		return []stubagent.Step{stubagent.RefuseAnswer("refuse-1", notReady)}
	}))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	_, before := c.call("GetTask", obj{"id": taskID})

	got := answerAsync(t, c, reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"PostgreSQL"})))
	expectNoAnswer(t, got)

	// The held send holds no lock: the task can be read, and a new task started in another context.
	if _, held := c.call("GetTask", obj{"id": taskID}); describe(held) != "task:INPUT_REQUIRED" {
		t.Errorf("task while the answer is held = %s, want task:INPUT_REQUIRED", describe(held))
	}
	agent.Next(stubagent.Text("meanwhile"))
	if got := taskText(last(c.send(msg("m3", "other", "")))); got != "meanwhile" {
		t.Errorf("a send to another context while the answer is held said %q, want %q", got, "meanwhile")
	}

	s.Release("refuse-1")
	evs := receive(t, got)

	if len(evs) != 1 {
		t.Fatalf("the refused answer had %d events, want one error: %v", len(evs), evs)
	}
	info, _ := dig(evs[0], "error", "data").([]any)
	if dig(evs[0], "error", "code") != float64(-32004) || dig(evs[0], "error", "message") != notReady ||
		len(info) != 1 || dig(info[0], "reason") != "UNSUPPORTED_OPERATION" {
		t.Errorf("refused answer = %v, want -32004 %q, UNSUPPORTED_OPERATION", evs[0], notReady)
	}
	_, after := c.call("GetTask", obj{"id": taskID})
	histBefore, _ := dig(before, "result", "history").([]any)
	histAfter, _ := dig(after, "result", "history").([]any)
	if describe(after) != "task:INPUT_REQUIRED" || len(histAfter) != len(histBefore) ||
		dig(after, "result", "status", "message", "metadata", hitlURI, "id") != request["id"] {
		t.Errorf("task after the refusal = %s with %d messages and request %v; want INPUT_REQUIRED, %d messages and the same request",
			describe(after), len(histAfter), dig(after, "result", "status", "message", "metadata", hitlURI, "id"), len(histBefore))
	}
}

// A unary SendMessage answer is refused in the same way, as a JSON body.
func TestRefuseAnswerRefusesAUnaryAnswer(t *testing.T) {
	s, agent, c := newAgent(t)
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step {
		return []stubagent.Step{stubagent.RefuseAnswer("refuse-2", notReady)}
	}))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	s.Release("refuse-2") // a gate released before the answer arrives lets it straight through

	status, got := c.call("SendMessage", reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"})))

	if status != 200 || dig(got, "error", "code") != float64(-32004) || dig(got, "error", "message") != notReady {
		t.Errorf("unary answer: HTTP %d %v, want HTTP 200 and -32004 %q", status, got, notReady)
	}
}

// RefuseAnswer refuses an answer; in a turn that no answer resumed it has nothing to refuse, and fails the task so
// the mistake shows.
func TestRefuseAnswerOutsideAnAnswerFailsTheTask(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("hello"), stubagent.RefuseAnswer("unused", notReady), stubagent.Text(" never"))

	final := last(c.send(msg("m1", "go", "")))

	if _, _, state := ids(final); state != "TASK_STATE_FAILED" || taskText(final) != "hello" || statusText(final) == "" {
		t.Errorf("state %s, output %q, status message %q; want FAILED, the text before and a reason", state, taskText(final), statusText(final))
	}
}
