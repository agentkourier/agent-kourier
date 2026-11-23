// Command adapter serves a Claude Managed Agents agent as an A2A agent, on the kit's runtime (package runtime, which
// owns the command's flag, exit codes, settings, caller check, store, leader election and stop).
//
// It is configured through its environment (the kit's package settings holds the variables every add-on shares, package
// config the adapter's own, and package telemetry the OTEL_* ones) and reads credentials as the Anthropic Go SDK does
// (ANTHROPIC_API_KEY, or the active `ant auth login` profile). A setting it cannot use stops it at start, with an error
// that names the variable. It takes one flag, --version.
//
// With SQLite it is the only replica and leads at once. With Postgres its replicas elect a leader through the store
// (package cluster). The leader restores each A2A context's session from the store and reconciles it with the agent's
// unarchived sessions in CMA, fails the tasks a restart or a takeover cut off, starts the store's upkeep, and only then
// serves A2A requests; a standby forwards them to it. A2A requests and the card must carry one of the tokens in
// AUTH_TOKEN_FILE; /healthz, /readyz and /metrics are open. A replica that loses the lease exits with
// cluster.ExitLeadershipLost (5).
package main

import (
	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

func main() { runtime.Main(addon()) }

// addon is the adapter at version, over the Managed Agents API.
func addon() runtime.Addon[*store.Store] { return adapter.Addon(version, newBackend) }

// newBackend is the Managed Agents API over the Anthropic Go SDK, which reads its own credentials and
// ANTHROPIC_BASE_URL, for the agent and environment cfg names.
func newBackend(cfg config.Config) adapter.Backend {
	return &adapter.SDK{Client: anthropic.NewClient(), AgentID: cfg.AgentID, EnvironmentID: cfg.EnvironmentID}
}
