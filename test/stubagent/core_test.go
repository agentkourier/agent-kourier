package stubagent_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

func TestAgentCardListsTheHITLExtension(t *testing.T) {
	s, _, _ := newAgent(t)
	url := s.AgentURL("kagent", "test")

	status, raw := do(t, http.MethodGet, url+"/.well-known/agent-card.json", nil, http.Header{})
	var card obj
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}

	ifaces, _ := card["supportedInterfaces"].([]any)
	exts, _ := dig(card, "capabilities", "extensions").([]any)
	if status != http.StatusOK || len(ifaces) != 1 || dig(ifaces[0], "url") != url || dig(ifaces[0], "protocolBinding") != "JSONRPC" {
		t.Errorf("card interfaces = %v (HTTP %d), want one JSONRPC interface at %s", ifaces, status, url)
	}
	if len(exts) != 1 || dig(exts[0], "uri") != stubagent.HITLExtension {
		t.Errorf("card extensions = %v, want the kagent HITL extension", exts)
	}
}

func TestUnknownRoutesAnswer404(t *testing.T) {
	s, _, _ := newAgent(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/agents/kagent/nobody/.well-known/agent-card.json"},
		{http.MethodPost, "/agents/kagent/nobody"},
		{http.MethodGet, "/agents/kagent/test"},
		{http.MethodPost, "/agents/kagent/test/.well-known/agent-card.json"},
		{http.MethodPost, "/elsewhere"},
	} {
		if status, _ := do(t, tc.method, s.URL()+tc.path, nil, http.Header{}); status != http.StatusNotFound {
			t.Errorf("%s %s: HTTP %d, want 404", tc.method, tc.path, status)
		}
	}
}

func TestRPCErrorsCarryErrorInfoOverHTTP200(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	_, _, c := newAgent(t, stubagent.WithClock(func() time.Time { return now }))

	status, body := c.call("Nope", obj{})

	if status != http.StatusOK || body["id"] != "1" || dig(body, "error", "code") != float64(-32601) {
		t.Fatalf("Nope = HTTP %d %v, want HTTP 200, the request id and error -32601", status, body)
	}
	info, _ := dig(body, "error", "data").([]any)
	if len(info) != 1 || dig(info[0], "@type") != "type.googleapis.com/google.rpc.ErrorInfo" ||
		dig(info[0], "domain") != "a2a-protocol.org" || dig(info[0], "reason") != "METHOD_NOT_FOUND" ||
		dig(info[0], "metadata", "timestamp") != "2026-09-30T12:00:00Z" {
		t.Errorf("error data = %v, want one ErrorInfo with the clock's timestamp", info)
	}
}

func TestMalformedJSONIsAParseError(t *testing.T) {
	s, _, _ := newAgent(t)

	status, raw := do(t, http.MethodPost, s.AgentURL("kagent", "test"), "{not json", http.Header{})

	if !strings.Contains(string(raw), `"code":-32700`) || status != http.StatusOK {
		t.Errorf("malformed body = HTTP %d %s, want error -32700", status, raw)
	}
}
