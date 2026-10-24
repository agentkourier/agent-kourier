package stubagent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// answered is what a send made from another goroutine got: its events, or why it got none.
type answered struct {
	evs []obj
	err error
}

// answerAsync sends the answer to a paused task from another goroutine, and returns where its outcome arrives. The
// goroutine reports to the channel only, never to t, so it may outlive the test.
func answerAsync(c *client, params obj) <-chan answered {
	got := make(chan answered, 1)
	go func() {
		evs, err := c.trySend(params)
		got <- answered{evs, err}
	}()
	return got
}

// expectNoAnswer fails the test if the answer arrives within a short wait: the gate it waits at is shut.
func expectNoAnswer(t *testing.T, got <-chan answered) {
	t.Helper()
	select {
	case a := <-got:
		t.Fatalf("the answer was answered before its gate was released: %v %v", a.evs, a.err)
	case <-time.After(50 * time.Millisecond):
	}
}

// receive waits for the events of an answer, for a while.
func receive(t *testing.T, got <-chan answered) []obj {
	t.Helper()
	select {
	case a := <-got:
		if a.err != nil {
			t.Fatalf("answer: %v", a.err)
		}
		return a.evs
	case <-time.After(5 * time.Second):
		t.Fatal("no answer after the gate was released")
		return nil
	}
}

// callWithin is call with a deadline: a call that the server does not answer within d fails the test at once,
// instead of when the test times out.
func callWithin(t *testing.T, c *client, d time.Duration, method string, params any) obj {
	t.Helper()
	body := obj{"jsonrpc": "2.0", "id": "1", "method": method, "params": params}
	resp, err := (&http.Client{Timeout: d}).Do(request(t, http.MethodPost, c.url, body, c.hdr))
	if err != nil {
		t.Fatalf("%s within %s: %v", method, d, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ev obj
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		t.Fatalf("%s: decode: %v", method, err)
	}
	return ev
}

// stateWithin polls GetTask until the task is in state, and fails the test if it is not within a few seconds.
func stateWithin(t *testing.T, c *client, taskID, state string) {
	t.Helper()
	got := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got = describe(callWithin(t, c, time.Second, "GetTask", obj{"id": taskID})); got == state {
			return
		}
	}
	t.Fatalf("task is %s, want %s", got, state)
}

// waitReceived waits until the stub has received the send of message id, so a wait for its answer that follows is
// not over before the send arrived.
func waitReceived(t *testing.T, s *stubagent.Server, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rec := httptest.NewRecorder()
		s.ServeSendControl(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_control/send-gate", nil))
		var st stubagent.SendGateState
		if json.Unmarshal(rec.Body.Bytes(), &st) == nil && st.Sends[id] > 0 {
			return
		}
	}
	t.Fatalf("the stub did not receive send %s", id)
}

// RefuseAnswer, as the continuation of an Ask, holds the answer's send at its gate, without the server's lock,
// then refuses the answer with -32004 and its text. The task still waits for input and did not take the answer.
func TestRefuseAnswerRefusesTheAnswerOnceItsGateIsReleased(t *testing.T) {
	s, agent, c := newAgent(t)
	reached := make(chan struct{}) // the answer reached the agent: its script ran, and the send goes on to the gate
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step {
		close(reached)
		return []stubagent.Step{stubagent.RefuseAnswer("refuse-1", stubagent.NotReady)}
	}))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	_, before := c.call("GetTask", obj{"id": taskID})

	got := answerAsync(c, reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"PostgreSQL"})))
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not reach the agent")
	}
	expectNoAnswer(t, got)

	// The held send holds no lock: the task can be read, and a new task started in another context. A send that
	// waited holding the lock would leave GetTask unanswered, and fail the test here.
	if held := callWithin(t, c, 2*time.Second, "GetTask", obj{"id": taskID}); describe(held) != "task:INPUT_REQUIRED" {
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
	if dig(evs[0], "error", "code") != float64(-32004) || dig(evs[0], "error", "message") != stubagent.NotReady ||
		len(info) != 1 || dig(info[0], "reason") != "UNSUPPORTED_OPERATION" {
		t.Errorf("refused answer = %v, want -32004 %q, UNSUPPORTED_OPERATION", evs[0], stubagent.NotReady)
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

// A unary SendMessage answer waits at the gate in the same way, and is refused as a JSON body.
func TestRefuseAnswerRefusesAUnaryAnswer(t *testing.T) {
	s, agent, c := newAgent(t)
	reached := make(chan struct{})
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step {
		close(reached)
		return []stubagent.Step{stubagent.RefuseAnswer("refuse-2", stubagent.NotReady)}
	}))
	c = withHITL(c)
	_, taskID, ctxID, pause := paused(t, c)
	body := obj{"jsonrpc": "2.0", "id": "1", "method": "SendMessage",
		"params": reply("m2", "x", ctxID, taskID, answers(pause["id"].(string), []string{"MySQL"}))}
	req := request(t, http.MethodPost, c.url, body, c.hdr)
	got := make(chan answered, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- answered{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var ev obj
		if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
			got <- answered{err: fmt.Errorf("HTTP %d: decode: %w", resp.StatusCode, err)}
			return
		}
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("HTTP %d, want 200", resp.StatusCode)
		}
		got <- answered{[]obj{ev}, err}
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not reach the agent")
	}
	expectNoAnswer(t, got)

	s.Release("refuse-2")
	ev := receive(t, got)[0]

	if dig(ev, "error", "code") != float64(-32004) || dig(ev, "error", "message") != stubagent.NotReady {
		t.Errorf("unary answer: %v, want -32004 %q", ev, stubagent.NotReady)
	}
}

// RefuseAnswer refuses an answer; in a turn that no answer resumed it has nothing to refuse, and fails the task so
// the mistake shows.
func TestRefuseAnswerOutsideAnAnswerFailsTheTask(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("hello"), stubagent.RefuseAnswer("unused", stubagent.NotReady), stubagent.Text(" never"))

	final := last(c.send(msg("m1", "go", "")))

	if _, _, state := ids(final); state != "TASK_STATE_FAILED" || taskText(final) != "hello" || statusText(final) == "" {
		t.Errorf("state %s, output %q, status message %q; want FAILED, the text before and a reason", state, taskText(final), statusText(final))
	}
}
