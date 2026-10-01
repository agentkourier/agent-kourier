package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// scenarioAgents is the agent each Phase 1 E2E scenario drives in the cluster; the table is in
// the stubagent package doc.
var scenarioAgents = map[string]string{
	"S1":  "spike-slow",
	"S2":  "scenario-memory",
	"S3":  "spike-ask",
	"S4":  "scenario-slow",
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

// S4 sends a second message while the first task runs, so the task must outlast courier's round
// trip through Slack and the cluster by a wide margin at the tick the Deployment runs.
func TestSlowAgentOutlastsTheDefaultTickByAWideMargin(t *testing.T) {
	const window = 5 * time.Second
	if got := time.Duration(stubagent.ScenarioSlowTicks-1) * defaultTick; got < window {
		t.Errorf("scenario-slow runs %s at the default tick, want at least %s", got, window)
	}
}
