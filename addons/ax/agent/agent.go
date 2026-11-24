// Package agent is the ADK agent as the kit's runtime runs it (package runtime): the add-on (Addon), its backend, whose
// turns are the ADK agent's, and the hooks the runtime finds on it. An ask_user question is a plain-text pause that the
// next message on the task answers; an answer that is not text is refused before the turn starts.
//
// Standalone (RUNNER_URL empty), the runtime answers /readyz. Beside the AX task runner (RUNNER_URL set), /readyz and
// /metadata/ are the runner's, proxied to it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/auth/oidc"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// NewModel builds the agent's model from its settings, its calls observed by tel.
type NewModel func(ctx context.Context, cfg config.Config, tel *telemetry.Telemetry) (model.LLM, error)

// Addon is the agent, version version, as the kit's runtime runs it: named ax (metrics agentkourier_ax_*, spans
// agentkourier.ax.*, its process "agent"), with its own settings (package config), the OIDC check, its store on SQLite
// or Postgres, and a backend whose model newModel builds.
func Addon(version string, newModel NewModel) runtime.Addon[*store.Store] {
	return runtime.Addon[*store.Store]{
		Name: "ax", Noun: "agent", Version: version,
		Telemetry: TelemetrySpec(),
		Settings:  config.Vars(),
		OIDC: func(issuer, audience, subject string) (auth.Verifier, error) {
			return oidc.New(issuer, audience, subject)
		},
		Card: runtime.Card{Name: "ax-adk", Description: "An ADK agent on Amazon Bedrock or the Anthropic API, served over A2A"},
		Open: func(ctx context.Context, cfg settings.Config, opts runtime.StoreOptions) (*store.Store, error) {
			o := store.Options{Log: opts.Log, OnDeposed: opts.OnDeposed}
			if cfg.Store == settings.StorePostgres {
				return store.OpenPostgres(ctx, cfg.PostgresDSN, o)
			}
			return store.OpenSQLite(ctx, cfg.SQLitePath, o)
		},
		Build: func(ctx context.Context, env runtime.Env[*store.Store]) (runtime.Backend, error) {
			if newModel == nil {
				return nil, errors.New("the agent has no model")
			}
			return newBackend(ctx, config.From(env.Config, env.Settings), env, newModel)
		},
	}
}

// Backend is the agent's backend: the ADK agent's turns (TextExecutor), the count of the ADK sessions the runtime's
// upkeep deletes, and, beside the AX task runner, its readiness and metadata.
type Backend struct {
	*TextExecutor
	metrics *Metrics
	log     *slog.Logger
	idleTTL time.Duration
	runner  http.Handler // the AX task runner's metadata server; nil standalone
	attrs   []any
}

var (
	_ a2asrv.AgentExecutionCleaner = (*Backend)(nil)
	_ runtime.MessageValidator     = (*Backend)(nil)
	_ runtime.IdleReporter         = (*Backend)(nil)
	_ runtime.Readiness            = (*Backend)(nil)
	_ runtime.Routes               = (*Backend)(nil)
	_ runtime.Describer            = (*Backend)(nil)
)

func newBackend(ctx context.Context, cfg config.Config, env runtime.Env[*store.Store], newModel NewModel) (*Backend, error) {
	metrics, err := NewMetrics(env.Telemetry)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	b := &Backend{metrics: metrics, log: env.Log, idleTTL: cfg.SessionIdleTTL,
		attrs: []any{"model", cfg.ModelName, "region", cfg.ModelRegion, "runner", !cfg.Standalone()}}
	if !cfg.Standalone() {
		u, err := url.Parse(cfg.RunnerURL)
		if err != nil {
			return nil, errors.New("RUNNER_URL is not a URL (its value is not shown)")
		}
		b.runner = httputil.NewSingleHostReverseProxy(u)
	}
	m, err := newModel(ctx, cfg, env.Telemetry)
	if err != nil {
		return nil, err
	}
	if b.TextExecutor, err = NewTextExecutor(m, env.Store.Sessions()); err != nil {
		return nil, err
	}
	return b, nil
}

// NewTextExecutor builds the ADK agent on m, with its sessions in sessions, and its ask_user tool, as the executor of
// its turns. Its cleanup callback does nothing: without one, ADK logs the cause of a failed execution or cancel at WARN,
// which a2a-go has logged already.
func NewTextExecutor(m model.LLM, sessions session.Service) (*TextExecutor, error) {
	return newTextExecutor(m, sessions, func(context.Context, *a2asrv.ExecutorContext, []*a2a.AgentCard, a2a.SendMessageResult, error) {})
}

// newTextExecutor is NewTextExecutor with cleaned as the ADK executor's cleanup callback.
func newTextExecutor(m model.LLM, sessions session.Service, cleaned adka2a.A2AExecutionCleanupCallback) (*TextExecutor, error) {
	if m == nil || sessions == nil {
		return nil, errors.New("the model and the sessions are required")
	}
	ask, err := functiontool.New(functiontool.Config{Name: "ask_user", Description: "Ask the user one question and wait for their answer.", IsLongRunning: true}, func(_ adkagent.Context, _ struct {
		Question string `json:"question"`
	}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("the ask_user tool: %w", err)
	}
	root, err := llmagent.New(llmagent.Config{Name: "ax_assistant", Description: "AX chat and investigation assistant", Model: m, Instruction: "Answer chat messages and investigate the alert information provided. Ask for missing information using ask_user. You have no access to external systems or mutation tools; distinguish supplied evidence from inference.", Tools: []tool.Tool{ask}})
	if err != nil {
		return nil, fmt.Errorf("the agent: %w", err)
	}
	return &TextExecutor{Inner: adka2a.NewExecutor(adka2a.ExecutorConfig{RunnerConfig: runner.Config{AppName: store.AppName, Agent: root, SessionService: sessions}, RunConfig: adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}, OutputMode: adka2a.OutputArtifactPerEvent, A2AExecutionCleanupCallback: cleaned})}, nil
}

// IdleContextsDeleted counts the ADK sessions a sweep deleted with their idle contexts, and logs them.
func (b *Backend) IdleContextsDeleted(n int, err error) {
	b.metrics.SessionsDeleted(DeleteIdle, n)
	if err == nil && n > 0 {
		b.log.Info("deleted the ADK sessions and tasks of idle contexts", "contexts", n, "session_idle_ttl", b.idleTTL)
	}
}

// Readyz is the AX task runner's /readyz beside it, and nil standalone (the runtime's).
func (b *Backend) Readyz() http.Handler {
	if b.runner == nil {
		return nil
	}
	return b.runner
}

// Routes proxies /metadata/ to the AX task runner beside it.
func (b *Backend) Routes(mux *http.ServeMux) {
	if b.runner != nil {
		mux.Handle("/metadata/", b.runner)
	}
}

// LogAttrs are the model, its region and whether the AX task runner is beside the agent.
func (b *Backend) LogAttrs() []any { return b.attrs }
