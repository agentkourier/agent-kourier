package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	"S12": "scenario-fails",
	"S13": "scenario-tools",
}

// serve starts the stub behind withControl. The stub closes before the server: that ends the held
// turns, so the server is not left waiting on their streams.
func serve(t *testing.T, tick time.Duration, requireAuth bool) *httptest.Server {
	t.Helper()
	stub := newStub(tick, requireAuth, "")
	srv := httptest.NewServer(withControl(stub))
	t.Cleanup(srv.Close)
	t.Cleanup(stub.Close)
	return srv
}

func TestStubServesEveryScenarioAgent(t *testing.T) {
	srv := serve(t, defaultTick, false)
	for id, agent := range scenarioAgents {
		url := srv.URL + "/agents/kagent/" + agent + "/.well-known/agent-card.json"
		if got := statusOf(t.Context(), t, http.MethodGet, url); got != http.StatusOK {
			t.Errorf("%s: card of %s answered %d; register the agent in newStub", id, agent, got)
		}
	}
}

// streamUntil reads the server-sent events until one contains want, and fails if the stream
// ends or the test's deadline passes first.
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

// sendGated sends scenario-gated a prompt that names gate, and returns its event stream.
func sendGated(ctx context.Context, t *testing.T, srv *httptest.Server, gate string) *bufio.Scanner {
	t.Helper()
	send := `{"jsonrpc":"2.0","id":"1","method":"SendStreamingMessage","params":{"message":` +
		`{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"go gate:` + gate + `"}]}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agents/kagent/scenario-gated", strings.NewReader(send))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return bufio.NewScanner(resp.Body)
}

func TestReleaseEndpointOpensTheGate(t *testing.T) {
	srv := serve(t, 0, false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sc := sendGated(ctx, t, srv, "g1")
	streamUntil(t, sc, "Holding on gate g1.")

	if got := statusOf(ctx, t, http.MethodPost, srv.URL+"/_control/release/g1"); got != http.StatusNoContent {
		t.Fatalf("release answered %d, want %d", got, http.StatusNoContent)
	}
	streamUntil(t, sc, "Released.")
}

// S4 releases the first task's gate and then the queued reply's, which may already be open when
// the reply's turn reaches it: a release that finds no gate yet must still count.
func TestReleaseBeforeTheTaskReachesItsGate(t *testing.T) {
	srv := serve(t, 0, false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if got := statusOf(ctx, t, http.MethodPost, srv.URL+"/_control/release/g2"); got != http.StatusNoContent {
		t.Fatalf("release answered %d, want %d", got, http.StatusNoContent)
	}

	streamUntil(t, sendGated(ctx, t, srv, "g2"), "Released.")
}

func TestControlSurfaceIsOnlyThePostedRelease(t *testing.T) {
	srv := serve(t, 0, true) // -require-auth must not gate it, as for the fake Slack
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/_control/release/g1", http.StatusNoContent},
		{http.MethodPost, "/_control/release/Run_1-a", http.StatusNoContent},
		{http.MethodGet, "/_control/release/g1", http.StatusNotFound},
		{http.MethodPost, "/_control/release/", http.StatusBadRequest},
		{http.MethodPost, "/_control/release/a%2Fb", http.StatusBadRequest},
		{http.MethodPost, "/_control/release/a/b", http.StatusBadRequest},
		{http.MethodPost, "/_control/release/a%20b", http.StatusBadRequest},
		{http.MethodPost, "/_control/other", http.StatusNotFound},
	} {
		if got := statusOf(t.Context(), t, tc.method, srv.URL+tc.path); got != tc.want {
			t.Errorf("%s %s answered %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestIDTagIsEmptyUnlessUniqueIDsIsSet: -unique-ids is what turns the tag on, and a start without it keeps the plain ids, which
// the spike's oracle and the unit tests count on; with it each start gets a tag of its own.
func TestIDTagIsEmptyUnlessUniqueIDsIsSet(t *testing.T) {
	plain, err := idTag(false)
	if err != nil || plain != "" {
		t.Errorf("idTag(false) = %q, %v; want no tag", plain, err)
	}
	first, err := idTag(true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := idTag(true)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second == "" || first == second {
		t.Errorf("idTag(true) made %q, then %q; want a tag for each start, and not the same", first, second)
	}
}

// TestEachStartMakesItsOwnIDs: two stubs built with a tag from idTag(true) answer the same request with different task ids. That
// the command passes the tag to the stub is run()'s one line; the fast tier restarts the stub's pod while Agent Kourier keeps its store,
// and an id of the earlier run must not come again.
func TestEachStartMakesItsOwnIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	idOf := func() string {
		t.Helper()
		tag, err := idTag(true)
		if err != nil {
			t.Fatal(err)
		}
		stub := newStub(0, false, tag)
		srv := httptest.NewServer(withControl(stub))
		t.Cleanup(srv.Close)
		t.Cleanup(stub.Close)
		send := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agents/kagent/scenario-memory", strings.NewReader(send))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Result struct {
				Task struct {
					ID string `json:"id"`
				} `json:"task"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Result.Task.ID == "" {
			t.Fatalf("decode the task: %v (id %q)", err, body.Result.Task.ID)
		}
		return body.Result.Task.ID
	}

	first, second := idOf(), idOf()

	if first == second {
		t.Errorf("two starts of the stub made the task id %q both times", first)
	}
}
