package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// proxyChildEnv names the variable that makes TestSlackClientSendsThroughTheProxyEnvironment act as the child process of
// TestSlackClientHonoursTheProxyEnvironment.
const proxyChildEnv = "AGENT_KOURIER_TEST_PROXY_CHILD"

// The install guide says the Slack Web API calls read the proxy variables, the reverse of the A2A transport, which dials
// directly (see TestTransportIgnoresProxyEnvironment in the kagentv1 package). net/http reads those variables once per
// process, so the test runs the client in a child process that starts with HTTP_PROXY set: the child asks for a host that
// resolves nowhere, and the proxy of this process must be the one that receives the request.
func TestSlackClientHonoursTheProxyEnvironment(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	// The child has a deadline of its own (-test.timeout), and the parent's context ends it at the same length, so that a hang fails the test.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSlackClientSendsThroughTheProxyEnvironment$", "-test.timeout=30s")
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.Contains(strings.ToLower(k), "proxy") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, proxyChildEnv+"=1", "HTTP_PROXY="+proxy.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child process failed: %v\n%s", err, out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "slack.proxy-test.invalid" {
		t.Errorf("the proxy received requests for %q; want the one the Slack client made, so that HTTP_PROXY is honoured", seen)
	}
}

// TestSlackClientSendsThroughTheProxyEnvironment is the child of the test above: on its own it does nothing.
func TestSlackClientSendsThroughTheProxyEnvironment(t *testing.T) {
	if os.Getenv(proxyChildEnv) == "" {
		t.Skip("runs as the child of TestSlackClientHonoursTheProxyEnvironment")
	}
	var saw slack.Config
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { saw = sc; return idleSlack{} }}
	conn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	built, err := cfg.newPlatform(conn, nil, nil)
	if err != nil || saw.HTTPClient == nil {
		t.Fatalf("newPlatform = %v; client config %+v", err, saw)
	}
	defer built.closeIdle()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://slack.proxy-test.invalid/api/auth.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := saw.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("the request did not reach the proxy: %v", err)
	}
	_ = resp.Body.Close()
}
