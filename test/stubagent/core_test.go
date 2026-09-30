package stubagent_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

func TestAgentCardAdvertisesKagentsInClusterURL(t *testing.T) {
	s, _, _ := newAgent(t)
	url := s.AgentURL("kagent", "test")

	status, raw := do(t, http.MethodGet, url+"/.well-known/agent-card.json", nil, http.Header{})
	var card obj
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}

	// kagent advertises its in-cluster address, not the one the client used (WP-02 section 8,
	// measured), so a client outside the cluster must override it. The stub does the same, so a
	// client that stopped overriding would fail against it.
	const inCluster = "http://kagent-controller.kagent.svc:8083/agents/kagent/test"
	ifaces, _ := card["supportedInterfaces"].([]any)
	exts, _ := dig(card, "capabilities", "extensions").([]any)
	if status != http.StatusOK || len(ifaces) != 1 || dig(ifaces[0], "url") != inCluster || dig(ifaces[0], "protocolBinding") != "JSONRPC" {
		t.Errorf("card interfaces = %v (HTTP %d), want one JSONRPC interface at %s", ifaces, status, inCluster)
	}
	if dig(ifaces[0], "url") == url {
		t.Errorf("the card advertises the stub's own address %s", url)
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

	if status != http.StatusOK || body["id"] != "1" || dig(body, "error", "code") != float64(-32601) || dig(body, "error", "message") != "method not found" {
		t.Fatalf("Nope = HTTP %d %v, want HTTP 200, the request id and error -32601 method not found", status, body)
	}
	info, _ := dig(body, "error", "data").([]any)
	if len(info) != 1 || dig(info[0], "@type") != "type.googleapis.com/google.rpc.ErrorInfo" ||
		dig(info[0], "domain") != "a2a-protocol.org" || dig(info[0], "reason") != "METHOD_NOT_FOUND" ||
		dig(info[0], "metadata", "timestamp") != "2026-09-30T12:00:00Z" {
		t.Errorf("error data = %v, want one ErrorInfo with the clock's timestamp", info)
	}
}

// TestEnvelopeErrors follows a2a-go v2.6.0 a2asrv/jsonrpc.go ServeHTTP: a body that is not JSON
// is -32700 "parse error: ...", a field of the wrong type is -32602, an id that is not a
// string, number or null, a jsonrpc that is not "2.0" or an empty method is -32600.
func TestEnvelopeErrors(t *testing.T) {
	s, _, _ := newAgent(t)
	for _, tc := range []struct {
		name, body string
		code       float64
		message    string // a prefix
		id         any
	}{
		{"not JSON", `{not json`, -32700, "parse error: ", nil},
		{"method of the wrong type", `{"jsonrpc":"2.0","id":"1","method":5}`, -32602, "invalid params: ", nil},
		{"wrong version", `{"jsonrpc":"1.0","id":"1","method":"GetTask"}`, -32600, "invalid request", "1"},
		{"no version", `{"id":"1","method":"GetTask"}`, -32600, "invalid request", "1"},
		{"object id", `{"jsonrpc":"2.0","id":{},"method":"GetTask"}`, -32600, "invalid request", nil},
		{"boolean id", `{"jsonrpc":"2.0","id":true,"method":"GetTask"}`, -32600, "invalid request", nil},
		{"empty method", `{"jsonrpc":"2.0","id":"1","method":""}`, -32600, "invalid request", "1"},
		{"no method", `{"jsonrpc":"2.0","id":"1"}`, -32600, "invalid request", "1"},
		{"number id is valid", `{"jsonrpc":"2.0","id":7,"method":"GetTask"}`, -32602, "invalid params", float64(7)},
		{"null id is valid", `{"jsonrpc":"2.0","id":null,"method":"GetTask"}`, -32602, "invalid params", nil},
		{"absent id is valid", `{"jsonrpc":"2.0","method":"GetTask"}`, -32602, "invalid params", nil},
	} {
		status, raw := do(t, http.MethodPost, s.AgentURL("kagent", "test"), tc.body, http.Header{})
		var got obj
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: bad JSON %q", tc.name, raw)
		}
		msg, _ := dig(got, "error", "message").(string)
		if status != http.StatusOK || dig(got, "error", "code") != tc.code || !strings.Contains(msg, tc.message) || got["id"] != tc.id {
			t.Errorf("%s = HTTP %d %s, want error %v %q with id %v", tc.name, status, raw, tc.code, tc.message, tc.id)
		}
	}
}
