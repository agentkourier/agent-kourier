package stubagent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

func TestSendCountsIncludeDuplicateMessageIDs(t *testing.T) {
	s, a, c := newAgent(t)
	a.Next(stubagent.Text("done"))
	c.call("SendMessage", msg("same-message", "hello", ""))
	c.call("SendMessage", msg("same-message", "hello", ""))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_control/send-gate", nil)
	rw := httptest.NewRecorder()
	if !s.ServeSendControl(rw, req) {
		t.Fatal("send state route not handled")
	}
	var state stubagent.SendGateState
	if err := json.Unmarshal(rw.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Sends["same-message"] != 2 {
		t.Fatalf("send attempts %d; want 2 including duplicate", state.Sends["same-message"])
	}
}
