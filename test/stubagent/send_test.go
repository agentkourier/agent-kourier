package stubagent_test

import (
	"net/http"
	"strings"
	"testing"
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

func TestSendRejectsWhatIsNotAMessage(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()
	for _, tc := range []struct {
		name   string
		params obj
	}{
		{"no message", obj{}},
		{"no messageId", msg("", "hi", "")},
	} {
		status, body := c.call("SendMessage", tc.params)
		if status != http.StatusOK || dig(body, "error", "code") != float64(-32602) {
			t.Errorf("%s = HTTP %d %v, want error -32602", tc.name, status, body)
		}
	}
	// The rejected messages did not use up the queued turn.
	if _, body := c.call("SendMessage", msg("m1", "hi", "")); dig(body, "error") != nil {
		t.Errorf("send after rejections = %v, want the queued turn to run", body)
	}
}

func TestUnscriptedTurnFailsLoudly(t *testing.T) {
	_, _, c := newAgent(t)

	_, body := c.call("SendMessage", msg("m1", "hi", ""))

	reason, _ := dig(body, "error", "message").(string)
	if dig(body, "error", "code") != float64(-32603) || !strings.Contains(reason, "stubagent") {
		t.Errorf("unscripted send = %v, want an internal error naming the stub", body)
	}
}
