package stubagent_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const completed = "TASK_STATE_COMPLETED"

func TestSendMessageRunsAScriptedTurn(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()

	status, body := c.call("SendMessage", msg("m1", "What is 2+2?", ""))

	tk := task(t, body)
	hist, _ := tk["history"].([]any)
	if status != http.StatusOK || dig(tk, "status", "state") != completed || tk["id"] == "" || tk["contextId"] == "" {
		t.Fatalf("SendMessage = HTTP %d %v, want the completed task", status, tk)
	}
	if len(hist) != 1 || dig(hist[0], "messageId") != "m1" || dig(hist[0], "taskId") != tk["id"] || dig(hist[0], "contextId") != tk["contextId"] {
		t.Errorf("history = %v, want the user's message once, with the task and context ids", hist)
	}
	if dig(tk, "metadata", "https://google.github.io/adk-docs/a2a/a2a-extension/") != true {
		t.Errorf("task metadata = %v, want ADK's marker", tk["metadata"])
	}
}

func TestSendWithContextIDContinuesSession(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()
	agent.Next()

	_, body := c.call("SendMessage", msg("m1", "hi", ""))
	first := task(t, body)
	_, body = c.call("SendMessage", msg("m2", "again", first["contextId"].(string)))

	second := task(t, body)
	if second["contextId"] != first["contextId"] || second["id"] == first["id"] {
		t.Errorf("continue: context %v task %v; first was %v and %v", second["contextId"], second["id"], first["contextId"], first["id"])
	}
}

// oneMessage builds params with a message of the given parts, and the ID and role a message needs.
func oneMessage(id string, parts ...obj) obj {
	return obj{"message": obj{"messageId": id, "role": "ROLE_USER", "parts": parts}}
}

// TestSendRejectsWhatIsNotAMessage follows a2a-go v2.6.0: a2asrv/handler.go handleSendMessage
// answers -32602 for a message with no ID, no parts or no role, and a2a/core.go
// (Part.UnmarshalJSON) for a part that does not have exactly one of text, raw, data and url.
func TestSendRejectsWhatIsNotAMessage(t *testing.T) {
	_, agent, c := newAgent(t)
	noRole := oneMessage("m1", obj{"text": "hi"})
	delete(noRole["message"].(obj), "role")
	noParts := oneMessage("m1")
	delete(noParts["message"].(obj), "parts")
	const onePart = "expected exactly one of text, raw, data, or url"
	for _, tc := range []struct {
		name    string
		params  obj
		message string // a prefix of the error message
	}{
		{"no message", obj{}, "message is required"},
		{"no messageId", msg("", "hi", ""), "message ID is required"},
		{"no parts", noParts, "message parts is required"},
		{"empty parts", oneMessage("m1"), "message parts is required"},
		{"no role", noRole, "message role is required"},
		{"a part with nothing in it", oneMessage("m1", obj{}), onePart},
		{"a part with two contents", oneMessage("m1", obj{"text": "hi", "data": obj{}}), onePart},
	} {
		agent.Next()
		status, body := c.call("SendMessage", tc.params)
		text, _ := dig(body, "error", "message").(string)
		if status != http.StatusOK || dig(body, "error", "code") != float64(-32602) || !strings.HasPrefix(text, tc.message) {
			t.Errorf("%s = HTTP %d %v, want error -32602 %q", tc.name, status, body, tc.message)
		}
	}
	// Each rejection left its queued turn alone, so seven are waiting: a part of any one kind
	// is accepted, and the turns run out only after the accepted ones.
	for i, part := range []obj{{"text": "hi"}, {"data": obj{"a": 1}}, {"raw": "aGk="}, {"url": "http://example.test/x"}} {
		if _, body := c.call("SendMessage", oneMessage("ok-"+strconv.Itoa(i), part)); dig(body, "error") != nil {
			t.Errorf("send with part %v = %v, want it accepted", part, body)
		}
	}
	if _, body := c.call("SendMessage", msg("last", "hi", "")); dig(body, "error") != nil {
		t.Errorf("a rejection used up a queued turn: %v", body)
	}
}

func TestUnscriptedTurnFailsLoudly(t *testing.T) {
	_, _, c := newAgent(t)

	_, body := c.call("SendMessage", msg("m1", "hi", ""))

	// A code kagent never sends, so it cannot be mistaken for one of its errors.
	reason, _ := dig(body, "error", "message").(string)
	info, _ := dig(body, "error", "data").([]any)
	if dig(body, "error", "code") != float64(-32099) || !strings.Contains(reason, "stubagent") ||
		len(info) != 1 || dig(info[0], "reason") != "STUBAGENT_UNSCRIPTED" {
		t.Errorf("unscripted send = %v, want -32099 STUBAGENT_UNSCRIPTED naming the stub", body)
	}
}

// TestReplyRunningTwiceForOneMessageStartsOneTask: Reply runs outside the lock, so two sends of
// one messageId can both be inside it (if it ran under the lock, the second would wait for the
// first and the test would time out). When both have their steps, the second finds the first's
// task and joins it instead of starting another.
func TestReplyRunningTwiceForOneMessageStartsOneTask(t *testing.T) {
	_, agent, c := newAgent(t)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	agent.Reply(func(string) []stubagent.Step {
		entered <- struct{}{}
		<-release
		return nil
	})
	results := make(chan obj, 2)
	send := func() { _, body := c.call("SendMessage", msg("m1", "hi", "")); results <- body }
	go send()
	go send()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the second send never reached Reply: Reply ran under the server's lock")
		}
	}
	close(release)

	a, b := task(t, <-results), task(t, <-results)

	if a["id"] != b["id"] {
		t.Errorf("one messageId started two tasks, %v and %v", a["id"], b["id"])
	}
}

func TestCloseDoesNotAnswerAWorkingTaskAsFinished(t *testing.T) {
	s, agent, c := newAgent(t)
	entered := make(chan struct{})
	agent.Reply(func(string) []stubagent.Step {
		close(entered)
		return []stubagent.Step{stubagent.Hold("never")}
	})
	got := make(chan string, 1)
	go func() {
		params := obj{"jsonrpc": "2.0", "id": "1", "method": "SendMessage", "params": msg("m1", "hi", "")}
		status, raw := do(t, http.MethodPost, c.url, params, http.Header{})
		got <- strconv.Itoa(status) + " " + string(raw)
	}()
	<-entered

	s.Close()

	if answer := <-got; !strings.HasPrefix(answer, "503 ") || strings.Contains(answer, `"result"`) {
		t.Errorf("SendMessage during Close = %q, want 503 and no result", answer)
	}
}
