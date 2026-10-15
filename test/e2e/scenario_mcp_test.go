//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"time"
)

// scenarioMCPCounters is the counter snapshot served by the full tier's scripted MCP server.
type scenarioMCPCounters map[string]map[string]int

// mcpCounters reads the full tier's per-model-scenario tool execution counters.
func (s *scenario) mcpCounters(ctx context.Context) (scenarioMCPCounters, error) {
	var out scenarioMCPCounters
	err := newClient(s.env.ScenarioMCPURL).do(ctx, http.MethodGet, "/_control/counters", nil, &out)
	return out, err
}

// mcpCount returns the number of calls for tool in scenario, including zero when the scenario has not run.
func mcpCount(counters scenarioMCPCounters, scenario, tool string) int {
	return counters[scenario][tool]
}

// waitMCPCount waits until the scripted MCP server has recorded exactly delta additional calls to tool.
func (s *scenario) waitMCPCount(scenario, tool string, before, delta int) {
	s.t.Helper()
	want := before + delta
	err := Wait(s.ctx, 30*time.Second, "the scripted MCP counter", func(ctx context.Context) (bool, error) {
		counters, err := s.mcpCounters(ctx)
		if err != nil {
			return false, err
		}
		return mcpCount(counters, scenario, tool) == want, nil
	})
	if err != nil {
		s.t.Fatalf("MCP %s/%s count did not become %d: %v", scenario, tool, want, err)
	}
}
