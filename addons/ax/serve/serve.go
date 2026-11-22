// Package serve runs the agent from its settings: the caller check, the store, the settlement of the turns a restart
// cut off, the A2A endpoint and its card, /healthz and /readyz, and the store's upkeep, until its context ends.
//
// Standalone (RUNNER_URL empty), the agent answers /readyz itself: 200 from the moment it listens, which is after the
// store is open and migrated and the cut-off turns are settled, until it starts to shut down. Beside the AX task runner
// (RUNNER_URL set), /readyz and /metadata/ are the runner's, proxied to it.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/auth"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

// shutdownTimeout bounds the wait for the requests in flight at a graceful stop.
const shutdownTimeout = 5 * time.Second

// readHeaderTimeout bounds how long a caller may take to send a request's headers before the server closes the
// connection.
const readHeaderTimeout = 10 * time.Second

// Options are what Run takes beside the settings and the model.
type Options struct {
	// Version is the agent's, which the card names and the log reports.
	Version string
	// Log receives the agent's log. Nil is slog.Default().
	Log *slog.Logger
	// Routes, if set, adds routes to the server's mux before it serves (the e2e agent's test controls).
	Routes func(*http.ServeMux)
	// Listening, if set, is told the address the agent listens on, once it does.
	Listening func(net.Addr)
}

// Run serves the agent that cfg describes, with m as its model, until ctx ends (it then returns nil, after a graceful
// stop) or a dependency fails.
func Run(ctx context.Context, cfg config.Config, m model.LLM, opts Options) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	check, watch, err := callerCheck(cfg, log)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg, store.Options{Log: log})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	n, err := st.SettleInterrupted(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Info("failed the turns a restart cut off", "tasks", n)
	}
	srv, err := agent.New(agent.Config{Model: m, Store: st, Check: check, PublicURL: cfg.PublicURL, Version: opts.Version})
	if err != nil {
		return err
	}
	var ready atomic.Bool
	mux, err := Mux(srv.Handler, cfg.RunnerURL, ready.Load)
	if err != nil {
		return err
	}
	if opts.Routes != nil {
		opts.Routes(mux)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()
	// The token watch and the upkeep stop, and are waited for, before the deferred Close of the store.
	background, stopBackground := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		stopBackground()
		wg.Wait()
	}()
	if watch != nil {
		wg.Go(func() { watch(background) })
	}
	upkeep := &agent.Upkeep{Store: st, TaskRetention: cfg.TaskRetention, SessionIdleTTL: cfg.SessionIdleTTL, Log: log}
	wg.Go(func() { upkeep.Run(background) })
	ready.Store(true)
	if opts.Listening != nil {
		opts.Listening(ln.Addr())
	}
	log.Info("agent listening", "version", opts.Version, "addr", ln.Addr().String(), "card", cfg.PublicURL+agent.CardPath,
		"model", cfg.ModelName, "region", cfg.ModelRegion, "store", cfg.Store, "auth", authMode(cfg), "auth_header", cfg.AuthHeader,
		"runner", !cfg.Standalone(), "task_retention", cfg.TaskRetention, "session_idle_ttl", cfg.SessionIdleTTL)

	var failure error
	select {
	case <-ctx.Done():
	case err := <-served:
		failure = fmt.Errorf("serve: %w", err)
	}
	ready.Store(false)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(drain); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("stop serving", "error", err)
	}
	return failure
}

// Mux serves a2a, the A2A endpoint and its card behind the caller check, with /healthz and /readyz. With runnerURL
// set, /readyz and /metadata/ are proxied to the AX task runner there; without it, /readyz answers 200 while ready
// says so and 503 otherwise.
func Mux(a2a http.Handler, runnerURL string, ready func() bool) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.Handle(agent.A2APath, a2a)
	mux.Handle(agent.A2APath+agent.CardPath, a2a)
	// Alive once it listens.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if runnerURL != "" {
		runner, err := url.Parse(runnerURL)
		if err != nil {
			return nil, errors.New("RUNNER_URL is not a URL (its value is not shown)")
		}
		proxy := httputil.NewSingleHostReverseProxy(runner)
		mux.Handle("/metadata/", proxy)
		mux.Handle("/readyz", proxy)
		return mux, nil
	}
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux, nil
}

// callerCheck builds the check cfg names, and for the token file the watch that keeps its tokens current.
func callerCheck(cfg config.Config, log *slog.Logger) (*auth.Check, func(context.Context), error) {
	switch {
	case cfg.AuthDisabled:
		log.Warn("the caller check is off (AUTH_DISABLED=true): anything that reaches the loopback address can run the agent", "addr", cfg.ListenAddr)
		return &auth.Check{Verifier: auth.Open{}, Log: log}, nil, nil
	case cfg.OIDC():
		v, err := auth.NewOIDC(cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCSubject)
		if err != nil {
			return nil, nil, fmt.Errorf("OIDC: %w", err)
		}
		return &auth.Check{Verifier: v, Bearer: true, Log: log}, nil, nil
	default:
		tokens, err := auth.LoadTokens(cfg.AuthTokenFile, log)
		if err != nil {
			return nil, nil, fmt.Errorf("AUTH_TOKEN_FILE: %w", err)
		}
		check := &auth.Check{Verifier: auth.TokenVerifier{Header: cfg.AuthHeader, Tokens: tokens},
			Bearer: strings.EqualFold(cfg.AuthHeader, "Authorization"), Log: log}
		return check, func(ctx context.Context) { tokens.Watch(ctx, auth.ReloadInterval) }, nil
	}
}

func authMode(cfg config.Config) string {
	switch {
	case cfg.AuthDisabled:
		return "disabled"
	case cfg.OIDC():
		return "oidc"
	}
	return "token-file"
}

// openStore opens the store the configuration names.
func openStore(ctx context.Context, cfg config.Config, opts store.Options) (*store.Store, error) {
	if cfg.Store == config.StorePostgres {
		return store.OpenPostgres(ctx, cfg.PostgresDSN, opts)
	}
	return store.OpenSQLite(ctx, cfg.SQLitePath, opts)
}
