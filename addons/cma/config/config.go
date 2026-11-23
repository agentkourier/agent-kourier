// Package config reads the adapter's settings from its environment and checks them, so that a setting the adapter
// cannot use stops it at start with an error that names the variable. The kit's settings package reads the ones every
// add-on shares; this package holds the adapter's own. An error never shows the value of a variable that may hold a
// secret (POSTGRES_DSN, AUTH_HEADER).
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

// Spec is how the adapter reads its settings: the shared ones with the token check alone, and its own.
func Spec() settings.Spec {
	return settings.Spec{Name: "cma", Noun: "adapter", PortOnlyHosts: true,
		Vars: []settings.Var{agentID, environmentID, toolResultTimeout}}
}

// Config is the adapter's settings.
type Config struct {
	settings.Config
	// AgentID and EnvironmentID name the CMA agent the adapter runs and the environment its sessions run in.
	AgentID, EnvironmentID string
	// ToolResultTimeout is how long a turn waits, with no session event, for a sandbox tool's result; zero waits for ever.
	ToolResultTimeout time.Duration
}

// Parse reads the settings through getenv (os.Getenv in production), where an empty value is an unset one, and checks
// them.
func Parse(getenv func(string) string) (Config, error) {
	shared, own, err := settings.Parse(getenv, Spec())
	if err != nil {
		return Config{}, err
	}
	return Config{Config: shared, AgentID: agentID.From(own), EnvironmentID: environmentID.From(own),
		ToolResultTimeout: toolResultTimeout.From(own)}, nil
}
