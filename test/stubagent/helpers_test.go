package stubagent_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

type obj = map[string]any

// newAgent starts a stub with one agent, kagent/test, and returns a client for it.
func newAgent(t *testing.T, opts ...stubagent.Option) (*stubagent.Server, *stubagent.Agent, *client) {
	t.Helper()
	s := stubagent.New(opts...)
	t.Cleanup(s.Close)
	return s, s.Agent("kagent", "test"), &client{t: t, url: s.AgentURL("kagent", "test"), hdr: http.Header{}}
}

// client is a raw JSON-RPC client, so tests see exactly what is on the wire.
type client struct {
	t   *testing.T
	url string
	hdr http.Header
}

// call sends one request and returns the HTTP status and the JSON body.
func (c *client) call(method string, params any) (int, obj) {
	c.t.Helper()
	req := obj{"jsonrpc": "2.0", "id": "1", "method": method, "params": params}
	status, raw := do(c.t, http.MethodPost, c.url, req, c.hdr)
	var out obj
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%s: bad JSON %q: %v", method, raw, err)
	}
	return status, out
}

// do sends a request and returns the status and body. A nil body sends none, a string is sent
// as it is, and anything else as JSON.
func do(t *testing.T, method, url string, body any, hdr http.Header) (int, []byte) {
	t.Helper()
	var r io.Reader = http.NoBody
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header = hdr.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, raw
}

// dig walks nested maps by key; it returns nil when a key is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(obj)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
