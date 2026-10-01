package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scenarioAgents is the agent each Phase 1 E2E scenario drives in the cluster; the table is in
// the stubagent package doc.
var scenarioAgents = map[string]string{
	"S1":  "scenario-gated",
	"S2":  "scenario-memory",
	"S3":  "scenario-ask",
	"S4":  "scenario-gated",
	"S5":  "test-agent",
	"S11": "spike-approve",
}

func TestStubServesEveryScenarioAgent(t *testing.T) {
	srv := httptest.NewServer(newStub(defaultTick, false))
	t.Cleanup(srv.Close)
	for id, agent := range scenarioAgents {
		url := srv.URL + "/agents/kagent/" + agent + "/.well-known/agent-card.json"
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: card of %s answered %d; register the agent in newStub", id, agent, resp.StatusCode)
		}
	}
}

// streamUntil reads the server-sent events of resp until one contains want, and fails if the
// stream ends or the test's deadline passes first.
func streamUntil(t *testing.T, sc *bufio.Scanner, want string) {
	t.Helper()
	for sc.Scan() {
		if strings.Contains(sc.Text(), want) {
			return
		}
	}
	t.Fatalf("the stream ended or timed out before %q: %v", want, sc.Err())
}

// statusOf sends a request with no body and returns the status it is answered with.
func statusOf(ctx context.Context, t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestReleaseEndpointOpensTheGate(t *testing.T) {
	srv := httptest.NewServer(withControl(newStub(0, false)))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	send := `{"jsonrpc":"2.0","id":"1","method":"SendStreamingMessage","params":{"message":` +
		`{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"go gate:g1"}]}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agents/kagent/scenario-gated", strings.NewReader(send))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	streamUntil(t, sc, "Holding on gate g1.")

	if got := statusOf(ctx, t, http.MethodPost, srv.URL+"/_control/release/g1"); got != http.StatusNoContent {
		t.Fatalf("release answered %d, want %d", got, http.StatusNoContent)
	}
	streamUntil(t, sc, "Released.")
}

func TestControlSurfaceIsOnlyThePostedRelease(t *testing.T) {
	srv := httptest.NewServer(withControl(newStub(0, true))) // -require-auth must not gate it, as for the fake Slack
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/_control/release/g1", http.StatusNoContent},
		{http.MethodGet, "/_control/release/g1", http.StatusNotFound},
		{http.MethodPost, "/_control/release/", http.StatusNotFound},
		{http.MethodPost, "/_control/other", http.StatusNotFound},
	} {
		if got := statusOf(t.Context(), t, tc.method, srv.URL+tc.path); got != tc.want {
			t.Errorf("%s %s answered %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
}
