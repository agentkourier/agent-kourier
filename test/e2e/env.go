// Package e2e holds the helpers end-to-end scenarios use to drive and observe the test
// environment (spec 12.1). They are plain HTTP callers, unit-tested against in-process servers,
// so `make test` runs them. Scenarios that need a cluster are tagged `e2e` and run in-cluster,
// reaching the doubles by Service name: the fake Slack's control surface is unauthenticated, so
// nothing is published to the host.
package e2e

import "strings"

// Env locates the test doubles; the defaults are the fast tier's Service names.
type Env struct {
	SlackURL        string // the fake Slack: Web API and /_control/
	SinkURL         string // the sink receiver
	StubURL         string // the stub agent: /agents/ and /_control/release/
	AlertmanagerURL string // Alertmanager's API
	// ScenarioMCPURL is the full tier's scripted MCP server control surface. It is
	// kept in Env so a scenario can read execution counters without coupling the
	// test to a cluster-local hostname.
	ScenarioMCPURL string
	// AgentKourierURL is Agent Kourier's health and metrics port: the fast tier's Service of that port alone
	// (the chart's Service, with charts/agent-kourier/ci/fast-values.yaml).
	AgentKourierURL string
	// SignalerURL is the container signaler (test/signaler), which the fast tier brings up with two Agent Kourier replicas
	// and the failover scenario uses to freeze or kill a replica's process.
	SignalerURL string
	// Tier is the tier the runner is in, "fast" or "full", from E2E_TIER (hack/kind/e2e-run.sh sets it). It
	// has no default: a test that needs it must fail when it is missing, not guess.
	Tier string
	// ConfigSource is where the tier's Agent Kourier reads its config from, "files" (the ConfigMap) or "crds" (custom resources), from
	// E2E_CONFIG_SOURCE (hack/kind/e2e-run.sh reads it from the Deployment). The scenarios of the custom resources route skip unless it is crds.
	ConfigSource string
	// Phase is the part of a scenario that a script runs between helm upgrades, from E2E_PHASE: empty in an ordinary run, in which such a
	// scenario skips (hack/kind/crs-migration.sh sets it).
	Phase string
	// ConnectionNamespaces is the platform's admission list, from E2E_CONNECTION_NAMESPACES (hack/kind/e2e-run.sh reads it from the Deployment):
	// the namespaces whose ChatConnections the broker serves, comma separated, empty for none. The scenarios of several Slack apps skip unless it
	// holds team-a.
	ConnectionNamespaces string
}

// Admits says whether the platform admits the ChatConnections of namespace.
func (e Env) Admits(namespace string) bool {
	for _, ns := range strings.Split(e.ConnectionNamespaces, ",") {
		if ns != "" && ns == namespace {
			return true
		}
	}
	return false
}

// NewEnv reads E2E_SLACK_URL, E2E_SINK_URL, E2E_STUB_URL, E2E_ALERTMANAGER_URL and
// E2E_AGENTKOURIER_URL, E2E_SIGNALER_URL, E2E_TIER, E2E_CONFIG_SOURCE, E2E_PHASE and E2E_CONNECTION_NAMESPACES through getenv (os.Getenv in a runner); an unset or empty variable takes its
// default.
func NewEnv(getenv func(string) string) Env {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return Env{
		SlackURL:             get("E2E_SLACK_URL", "http://fakeslack.e2e.svc.cluster.local:8080"),
		SinkURL:              get("E2E_SINK_URL", "http://sink.e2e.svc.cluster.local:8080"),
		StubURL:              get("E2E_STUB_URL", "http://stubagent.e2e.svc.cluster.local:8080"),
		AlertmanagerURL:      get("E2E_ALERTMANAGER_URL", "http://alertmanager.monitoring.svc.cluster.local:9093"),
		ScenarioMCPURL:       get("E2E_SCENARIO_MCP_URL", "http://scenariomcp.kagent.svc.cluster.local:8082"),
		AgentKourierURL:      get("E2E_AGENTKOURIER_URL", "http://agent-kourier.agent-kourier.svc.cluster.local:8080"),
		SignalerURL:          get("E2E_SIGNALER_URL", "http://signaler.e2e.svc.cluster.local:8080"),
		Tier:                 getenv("E2E_TIER"),
		ConfigSource:         get("E2E_CONFIG_SOURCE", "files"),
		Phase:                getenv("E2E_PHASE"),
		ConnectionNamespaces: getenv("E2E_CONNECTION_NAMESPACES"),
	}
}
