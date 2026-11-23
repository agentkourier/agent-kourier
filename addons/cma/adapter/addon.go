package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

// NewBackend builds the Managed Agents API the adapter calls, for the agent and environment its settings name.
type NewBackend func(cfg config.Config) Backend

// Addon is the adapter, version version, as the kit's runtime runs it: named cma (metrics agentkourier_cma_*, spans
// agentkourier.cma.*, its process "adapter"), with its own settings (package config), the token check alone, its store
// on SQLite or Postgres, and a backend, the Executor, whose calls to the API newBackend builds, timed and traced
// (Observe).
func Addon(version string, newBackend NewBackend) runtime.Addon[*store.Store] {
	return runtime.Addon[*store.Store]{
		Name: "cma", Noun: "adapter", Version: version,
		Telemetry: TelemetrySpec(),
		Settings:  config.Vars(),
		Card: runtime.Card{Name: "claude-managed-agent", Description: "A Claude Managed Agents agent served over A2A",
			Extensions: []a2a.AgentExtension{{URI: HITLExtension, Description: "Tool approvals as structured pauses"}}},
		Open: func(ctx context.Context, cfg settings.Config, opts runtime.StoreOptions) (*store.Store, error) {
			o := store.Options{Log: opts.Log, OnDeposed: opts.OnDeposed}
			if cfg.Store == settings.StorePostgres {
				return store.OpenPostgres(ctx, cfg.PostgresDSN, o)
			}
			return store.OpenSQLite(ctx, cfg.SQLitePath, o)
		},
		Build: func(_ context.Context, env runtime.Env[*store.Store]) (runtime.Backend, error) {
			if newBackend == nil {
				return nil, errors.New("the adapter has no Managed Agents API")
			}
			cfg := config.From(env.Config, env.Settings)
			exec := &Executor{Backend: Observe(newBackend(cfg), env.Telemetry), Store: env.Store, IdleTTL: cfg.SessionIdleTTL,
				ToolResultTimeout: cfg.ToolResultTimeout, Log: env.Log}
			var err error
			if exec.Metrics, err = NewMetrics(env.Telemetry, exec.Contexts); err != nil {
				return nil, fmt.Errorf("metrics: %w", err)
			}
			return &addonBackend{Executor: exec, attrs: []any{"agent", cfg.AgentID, "environment", cfg.EnvironmentID,
				"tool_result_timeout", cfg.ToolResultTimeout}}, nil
		},
	}
}

// addonBackend is the executor as the runtime runs it, with what its "adapter listening" line reports. The executor's
// hooks are promoted.
type addonBackend struct {
	*Executor
	attrs []any
}

var (
	_ runtime.Restorer        = (*addonBackend)(nil)
	_ runtime.Interrupter     = (*addonBackend)(nil)
	_ runtime.ResumeValidator = (*addonBackend)(nil)
	_ runtime.ContextReleaser = (*addonBackend)(nil)
	_ runtime.TurnClassifier  = (*addonBackend)(nil)
	_ runtime.Describer       = (*addonBackend)(nil)
)

// LogAttrs are the agent, the environment and the tool result timeout.
func (b *addonBackend) LogAttrs() []any { return b.attrs }
