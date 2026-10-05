// Package e2e holds the helpers end-to-end scenarios use to drive and observe the test
// environment (spec 12.1). They are plain HTTP callers, unit-tested against in-process servers,
// so `make test` runs them. Scenarios that need a cluster are tagged `e2e` and run in-cluster,
// reaching the doubles by Service name: the fake Slack's control surface is unauthenticated, so
// nothing is published to the host.
package e2e

// Env locates the test doubles; the defaults are the fast tier's Service names.
type Env struct {
	SlackURL        string // the fake Slack: Web API and /_control/
	SinkURL         string // the sink receiver
	StubURL         string // the stub agent: /agents/ and /_control/release/
	AlertmanagerURL string // Alertmanager's API
	// CourierURL is courier's health and metrics port: the fast tier's Service of that port alone
	// (the chart's Service, with charts/courier/ci/fast-values.yaml). The Alertmanager receiver is not behind it.
	CourierURL string
	// Tier is the tier the runner is in, "fast" or "full", from E2E_TIER (hack/kind/e2e-run.sh sets it). It
	// has no default: a test that needs it must fail when it is missing, not guess.
	Tier string
}

// NewEnv reads E2E_SLACK_URL, E2E_SINK_URL, E2E_STUB_URL, E2E_ALERTMANAGER_URL and
// E2E_COURIER_URL and E2E_TIER through getenv (os.Getenv in a runner); an unset or empty variable takes its
// default.
func NewEnv(getenv func(string) string) Env {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return Env{
		SlackURL:        get("E2E_SLACK_URL", "http://fakeslack.e2e.svc.cluster.local:8080"),
		SinkURL:         get("E2E_SINK_URL", "http://sink.e2e.svc.cluster.local:8080"),
		StubURL:         get("E2E_STUB_URL", "http://stubagent.e2e.svc.cluster.local:8080"),
		AlertmanagerURL: get("E2E_ALERTMANAGER_URL", "http://alertmanager.monitoring.svc.cluster.local:9093"),
		CourierURL:      get("E2E_COURIER_URL", "http://courier.courier.svc.cluster.local:8080"),
		Tier:            getenv("E2E_TIER"),
	}
}
