package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// testToken is the one token in the file the tests below write: obviously not a real credential.
const testToken = "fake-token-for-tests"

// TestCallerCheckLimits holds how often the add-on reads its token file, which it watches at the kit's interval, to
// docs/reference/limits.md.
func TestCallerCheckLimits(t *testing.T) {
	limitsdoc.Duration(t, "cma.token-reload", auth.ReloadInterval)
}

// logSink is a log the server and the test may use at once.
type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startServing runs the adapter on SQLite with the settings in more, against a CMA that answers at once, until the test
// ends, and returns its base URL once it is ready. The adapter logs to log, as JSON.
func startServing(t *testing.T, more map[string]string, log io.Writer) string {
	t.Helper()
	cma := &fakeCMA{released: make(chan struct{})}
	cma.release()
	api := httptest.NewServer(cma)
	t.Cleanup(api.Close)
	t.Setenv("ANTHROPIC_BASE_URL", api.URL)
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")
	addr := freeAddr(t)
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "LISTEN_ADDR": addr,
		"SQLITE_PATH": filepath.Join(t.TempDir(), "cma.db")}
	maps.Copy(env, more)
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, cfg, telemetry.Config{}, slog.New(slog.NewJSONHandler(log, nil)), serveOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return after its context ended")
		}
	})
	base := "http://" + addr
	newProber().waitFor(t, base+"/readyz", http.StatusOK)
	return base
}

// tokenFile writes a token file that holds testToken, and returns its path.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// get answers a GET of url with header set: the status, the Bearer challenge, and the body.
func get(t *testing.T, url string, header http.Header) (status int, challenge, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	resp, err := newProber().c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), string(raw)
}

// The agent card is behind the caller check, as the A2A endpoint is: a caller without an accepted token is refused
// with 401, logged with the reason and the header, and counted by reason, and one with it gets the card, in the header
// the check reads.
func TestCardIsBehindTheCallerCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		header    string
		env       map[string]string
		accepted  http.Header
		wrong     http.Header
		challenge string
	}{
		{"authorization", "Authorization", map[string]string{},
			http.Header{"Authorization": {"Bearer " + testToken}}, http.Header{"Authorization": {"Bearer fake-wrong-token"}}, "Bearer"},
		{"named header", "X-Kourier-Token", map[string]string{"AUTH_HEADER": "X-Kourier-Token"},
			http.Header{"X-Kourier-Token": {testToken}}, http.Header{"Authorization": {"Bearer " + testToken}, "X-Kourier-Token": {"fake-wrong-token"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.env["AUTH_TOKEN_FILE"] = tokenFile(t)
			log := &logSink{}
			base := startServing(t, tc.env, log)
			card := base + a2aPath + adapter.CardPath
			for name, h := range map[string]http.Header{"no credentials": nil, "a wrong token": tc.wrong} {
				status, challenge, body := get(t, card, h)
				if status != http.StatusUnauthorized || challenge != tc.challenge || strings.Contains(body, "claude-managed-agent") {
					t.Errorf("card with %s = %d, challenge %q, body %q; want 401, challenge %q, no card", name, status, challenge, body, tc.challenge)
				}
			}
			if status, _, body := get(t, card, tc.accepted); status != http.StatusOK || !strings.Contains(body, `"name":"claude-managed-agent"`) {
				t.Fatalf("card with the token = %d %q, want 200 and the card", status, body)
			}
			for _, reason := range []string{"missing", "invalid"} {
				want := `"msg":"refused an A2A request","reason":"` + reason + `","header":"` + tc.header + `","remote":`
				if !strings.Contains(log.String(), want) {
					t.Errorf("the log lacks %s", want)
				}
			}
			if strings.Contains(log.String(), testToken) || strings.Contains(log.String(), "fake-wrong-token") {
				t.Error("the log shows a presented token")
			}
			_, _, metrics := get(t, base+"/metrics", nil)
			for _, want := range []string{`agentkourier_cma_auth_refused_total{reason="missing"} 1`, `agentkourier_cma_auth_refused_total{reason="invalid"} 1`} {
				if !strings.Contains(metrics, want+"\n") {
					t.Errorf("/metrics lacks %s", want)
				}
			}
		})
	}
}

// With the check off, the card needs no token.
func TestCardWithTheCallerCheckOff(t *testing.T) {
	base := startServing(t, map[string]string{"AUTH_DISABLED": "true"}, io.Discard)
	if status, _, body := get(t, base+a2aPath+adapter.CardPath, nil); status != http.StatusOK || !strings.Contains(body, `"name":"claude-managed-agent"`) {
		t.Fatalf("card = %d %q, want 200 and the card", status, body)
	}
}

// call sends the JSON-RPC request method with params to the A2A endpoint at base, with no credentials, and returns the
// code of the response's error, 0 for none. It names the protocol version the server serves, as Agent Kourier's client
// does: without it the request handler answers as to a 0.3 client, with -32009 (version not supported). A response
// that is not JSON-RPC fails t.
func call(t *testing.T, base, method string, params any) (code int) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+a2aPath, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(a2a.SvcParamVersion, string(a2a.Version))
	resp, err := newProber().c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("%s answered %d with a body that is not JSON-RPC: %v", method, resp.StatusCode, err)
	}
	if answer.Error != nil {
		return answer.Error.Code
	}
	return 0
}

// With the check off, an A2A call without credentials reaches the request handler: the kit's Interceptor refuses a call
// that carries no caller, so the Open check, which makes it run as auth.Caller, is what keeps /a2a answering.
func TestA2AWithTheCallerCheckOff(t *testing.T) {
	base := startServing(t, map[string]string{"AUTH_DISABLED": "true"}, io.Discard)
	// -32001 is A2A's TaskNotFoundError; a call the Interceptor refused would get -31401, unauthenticated.
	if code := call(t, base, "GetTask", &a2a.GetTaskRequest{ID: "no-such-task"}); code != -32001 {
		t.Fatalf("GetTask of an unknown task: error code %d, want -32001 (task not found)", code)
	}
}
