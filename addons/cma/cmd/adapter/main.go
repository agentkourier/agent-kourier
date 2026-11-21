// Command adapter serves a Claude Managed Agents agent as an A2A agent.
//
// It is configured through its environment (package config holds every variable and its check) and reads credentials
// as the Anthropic Go SDK does (ANTHROPIC_API_KEY, or the active `ant auth login` profile). A setting it cannot use
// stops it at start, with an error that names the variable.
//
// At start it opens its store (SQLite, the default, or Postgres), restores each A2A context's session from the store
// and reconciles it with the agent's unarchived sessions in CMA, fails the tasks a restart cut off, and only then
// serves. A2A requests must carry one of the tokens in AUTH_TOKEN_FILE; the card, /healthz and /readyz are open.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
	"github.com/agentkourier/agent-kourier/addons/cma/auth"
	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("adapter stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Parse(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	var tokens *auth.Tokens
	if !cfg.AuthDisabled {
		if tokens, err = auth.LoadTokens(cfg.AuthTokenFile, log); err != nil {
			return fmt.Errorf("AUTH_TOKEN_FILE: %w", err)
		}
	}

	st, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	exec := &adapter.Executor{
		Backend:           &adapter.SDK{Client: anthropic.NewClient(), AgentID: cfg.AgentID, EnvironmentID: cfg.EnvironmentID},
		Store:             st,
		IdleTTL:           cfg.SessionIdleTTL,
		ToolResultTimeout: cfg.ToolResultTimeout,
		TaskRetention:     cfg.TaskRetention,
		Log:               log,
	}
	if err := exec.Restore(ctx); err != nil {
		return err
	}
	if err := exec.Settle(ctx); err != nil {
		return err
	}
	go exec.Run(ctx)

	rpc := a2asrv.NewJSONRPCHandler(adapter.NewRequestHandler(exec, a2asrv.WithCallInterceptors(auth.Interceptor{})))
	if tokens != nil {
		go tokens.Watch(ctx, auth.ReloadInterval)
		rpc = (&auth.Check{Header: cfg.AuthHeader, Tokens: tokens, Log: log}).Wrap(rpc)
	} else {
		log.Warn("the caller check is off (AUTH_DISABLED=true): anything that reaches the loopback address can run the agent", "addr", cfg.ListenAddr)
	}
	card := adapter.Card("claude-managed-agent", "A Claude Managed Agents agent served over A2A", cfg.PublicURL)
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: adapter.Mux("/a2a", card, rpc), ReadHeaderTimeout: 10 * time.Second}

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Info("adapter listening", "addr", cfg.ListenAddr, "card", cfg.PublicURL+"/.well-known/agent-card.json",
		"agent", cfg.AgentID, "environment", cfg.EnvironmentID, "store", cfg.Store, "auth_header", cfg.AuthHeader,
		"auth_disabled", cfg.AuthDisabled, "session_idle_ttl", cfg.SessionIdleTTL, "tool_result_timeout", cfg.ToolResultTimeout,
		"task_retention", cfg.TaskRetention)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// openStore opens the store the configuration names. The leader election's settings (LEASE_*, ADVERTISE_URL,
// POD_NAME) are read and checked by config, and not used yet: nothing stops two adapters on one Postgres.
func openStore(ctx context.Context, cfg config.Config, log *slog.Logger) (*store.Store, error) {
	opts := store.Options{Log: log}
	if cfg.Store == config.StorePostgres {
		return store.OpenPostgres(ctx, cfg.PostgresDSN, opts)
	}
	return store.OpenSQLite(ctx, cfg.SQLitePath, opts)
}
