// Package config holds the adapter's own settings, which the kit's runtime reads from the environment with the ones
// every add-on shares (package settings) and checks, so that a setting the adapter cannot use stops it at start with an
// error that names the variable. An error never shows the value of a variable that may hold a secret (POSTGRES_DSN,
// AUTH_HEADER).
package config

import (
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

// The adapter's own variables.
var (
	agentID           = settings.Required("CMA_AGENT_ID")
	environmentID     = settings.Required("CMA_ENVIRONMENT_ID")
	toolResultTimeout = settings.Duration("TOOL_RESULT_TIMEOUT", "10m")
)

// Vars are the adapter's own variables, in the order they are read (runtime.Addon.Settings).
func Vars() []settings.Var { return []settings.Var{agentID, environmentID, toolResultTimeout} }

// Config is the adapter's settings.
type Config struct {
	settings.Config
	// AgentID and EnvironmentID name the CMA agent the adapter runs and the environment its sessions run in.
	AgentID, EnvironmentID string
	// ToolResultTimeout is how long a turn waits, with no session event, for a sandbox tool's result; zero waits for ever.
	ToolResultTimeout time.Duration
}

// From is the adapter's settings: shared, the ones every add-on has, and its own, as the kit's runtime read them (Vars).
func From(shared settings.Config, own settings.Values) Config {
	return Config{Config: shared, AgentID: agentID.From(own), EnvironmentID: environmentID.From(own),
		ToolResultTimeout: toolResultTimeout.From(own)}
}
