// Command adapter serves a Claude Managed Agents agent as an A2A agent.
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
// AUTH_TOKEN_FILE; /healthz, /readyz and /metrics are open.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

// The exit codes. A replica that loses the leadership exits with cluster.ExitLeadershipLost (5).
const (
	exitOK      = 0
	exitFailure = 1 // a dependency failed: the store, CMA, the listener
	exitUsage   = 2 // a flag, an argument or a setting is wrong
)

// a2aPath is the JSON-RPC endpoint's path; the card is under it.
const a2aPath = "/a2a"

// shutdownTimeout bounds the wait for the requests in flight at a graceful stop, and releaseTimeout the hand-over of the
// lease after it. Together they stay well inside Kubernetes's default grace period of 30 s.
const (
	shutdownTimeout = 5 * time.Second
	releaseTimeout  = 3 * time.Second
)

// readHeaderTimeout bounds how long a caller may take to send a request's headers before the server closes the connection.
const readHeaderTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run executes the command and returns its exit code.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("agent-kourier-cma", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		// The argument is not echoed: a person may have typed a token in its place.
		_, _ = fmt.Fprintln(stderr, "agent-kourier-cma: the adapter takes no arguments; it is configured by its environment")
		return exitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return exitOK
	}
	cfg, err := config.Parse(getenv)
	if err == nil {
		var traces telemetry.Config
		if traces, err = telemetry.LoadConfig(adapter.TelemetrySpec().Name, getenv); err == nil {
			log := slog.New(telemetry.NewLogHandler(stderr, cfg.LogLevel))
			slog.SetDefault(log)
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if err := serve(ctx, cfg, traces, log, serveOptions{}); err != nil {
				log.Error("adapter stopped", "error", err)
				return exitFailure
			}
			return exitOK
		}
	}
	slog.New(telemetry.NewLogHandler(stderr, slog.LevelInfo)).Error("configuration", "error", err)
	return exitUsage
}

// serveOptions are what a test may add to serve.
type serveOptions struct {
	// routes, if set, adds routes to the server's mux before it serves.
	routes func(*http.ServeMux)
}

// serve runs the adapter until ctx ends or a dependency fails.
func serve(ctx context.Context, cfg config.Config, traces telemetry.Config, log *slog.Logger, opts serveOptions) error {
	if traces.Endpoint != "" {
		telemetry.InstallDiagnostics(log)
	}
	provider, shutdownTraces, err := traces.Provider(ctx, version, log)
	if err != nil {
		return fmt.Errorf("traces: %w", err)
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = shutdownTraces(flush)
	}()
	tel, err := telemetry.New(adapter.TelemetrySpec(), provider)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	var tokens *auth.Tokens
	if !cfg.AuthDisabled {
		if tokens, err = auth.LoadTokens(cfg.AuthTokenFile, log); err != nil {
			return fmt.Errorf("AUTH_TOKEN_FILE: %w", err)
		}
	}

	lost := &cluster.FailStop{Log: log, Exit: os.Exit}
	st, err := openStore(ctx, cfg, store.Options{Log: log, OnDeposed: lost.Deposed})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	exec := &adapter.Executor{
		Backend:           adapter.Observe(&adapter.SDK{Client: anthropic.NewClient(), AgentID: cfg.AgentID, EnvironmentID: cfg.EnvironmentID}, tel),
		Store:             st,
		IdleTTL:           cfg.SessionIdleTTL,
		ToolResultTimeout: cfg.ToolResultTimeout,
		TaskRetention:     cfg.TaskRetention,
		Log:               log,
		Telemetry:         tel,
	}
	if exec.Metrics, err = adapter.NewMetrics(tel, exec.Contexts); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	rpc := a2asrv.NewJSONRPCHandler(adapter.NewRequestHandler(exec, auth.Interceptor{}))
	// The background work ends when serve returns, whether or not ctx has ended, and before the store closes.
	var background sync.WaitGroup
	defer background.Wait()
	backgroundCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	refused := func(reason auth.Reason) { tel.AuthRefused(string(reason)) }
	check := &auth.Check{Verifier: auth.Open{}, Log: log, Refused: refused}
	if tokens != nil {
		background.Go(func() { tokens.Watch(backgroundCtx, auth.ReloadInterval) })
		check = &auth.Check{Verifier: auth.TokenVerifier{Header: cfg.AuthHeader, Tokens: tokens},
			Bearer: strings.EqualFold(cfg.AuthHeader, "Authorization"), Header: cfg.AuthHeader, Log: log, Refused: refused}
	} else {
		log.Warn("the caller check is off (AUTH_DISABLED=true): anything that reaches the loopback address can run the agent", "addr", cfg.ListenAddr)
	}
	card := adapter.Card("claude-managed-agent", "A Claude Managed Agents agent served over A2A", cfg.PublicURL, version)
	status := &cluster.Status{}
	router := &cluster.Router{Local: telemetry.Extract(adapter.Mux(a2aPath, card, rpc, check)), Status: status,
		Addon: adapter.TelemetrySpec().Name, Noun: "adapter", RetryAfter: cfg.Lease.RetryPeriod, Telemetry: tel, Log: log}
	var elector *cluster.Elector
	if cfg.Store == settings.StorePostgres {
		elector = &cluster.Elector{Lease: st, Holder: cfg.PodName, Address: cfg.AdvertiseURL, LeaseDuration: cfg.Lease.Duration,
			RenewDeadline: cfg.Lease.RenewDeadline, RetryPeriod: cfg.Lease.RetryPeriod, Status: status, Telemetry: tel, Log: log}
		router.Leaders = elector
		// One try at the lease before listening: a replica that takes it leads from its first answer, so /readyz cannot
		// answer 200 before its takeover is done; one that does not is a standby, ready at once, and campaigns in Run.
		elector.TryLeadOnce(ctx)
	} else {
		// The only replica leads before it listens, so /readyz cannot answer 200 before its takeover is done.
		status.Lead()
		tel.SetLeader(true)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		if elector != nil {
			release, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			if err := elector.Release(release); err != nil {
				log.Warn("hand the lease over; a standby takes it once it expires", "error", err)
			}
		}
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	var stopping atomic.Bool
	handler := mux(router, func() bool { return !stopping.Load() && status.Ready() }, tel)
	if opts.routes != nil {
		opts.routes(handler)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("adapter listening", "version", version, "addr", cfg.ListenAddr, "card", cfg.PublicURL+adapter.CardPath,
		"agent", cfg.AgentID, "environment", cfg.EnvironmentID, "store", cfg.Store, "auth_header", cfg.AuthHeader,
		"auth_disabled", cfg.AuthDisabled, "session_idle_ttl", cfg.SessionIdleTTL, "tool_result_timeout", cfg.ToolResultTimeout,
		"task_retention", cfg.TaskRetention, "pod", cfg.PodName, "traces", traces.Endpoint != "")

	lead := func(ctx context.Context) error { return takeOver(ctx, exec, status, log) }
	// The election outlives ctx until the server has stopped, so that the lease is renewed while requests drain and a
	// standby does not take over while this replica still writes.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()
	led := make(chan error, 1)
	go func() {
		if elector == nil {
			led <- lead(electionCtx)
			return
		}
		led <- elector.Run(electionCtx, lead)
	}()

	var failure error
	ledDone := false
	select {
	case <-ctx.Done():
	case err := <-served:
		failure = fmt.Errorf("serve: %w", err)
	case err := <-led:
		ledDone = true
		if errors.Is(err, cluster.ErrLeadershipLost) {
			lost.LeaseLost()
		}
		failure = err
	}
	stopping.Store(true)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("stop serving", "error", err)
	}
	stopElection()
	if !ledDone {
		// The election's context has ended, so Run, or the takeover, returns once the leader's work (the upkeep) has
		// stopped, before the lease is released and the store closed; a takeover it cut short says so.
		if err := <-led; err != nil {
			log.Info("stopped before the takeover was done", "error", err)
		}
	}
	if elector != nil {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := elector.Release(release); err != nil {
			log.Warn("hand the lease over; a standby takes it once it expires", "error", err)
		}
	}
	return failure
}

// leaderWork is the executor's side of a takeover.
type leaderWork interface {
	// Restore reconciles the store with CMA.
	Restore(ctx context.Context) error
	// Settle fails the turns a restart or a takeover cut off.
	Settle(ctx context.Context) error
	// Run is the store's upkeep, until ctx ends.
	Run(ctx context.Context)
}

// takeOver is the leader's work: reconcile, fail the interrupted turns, start the upkeep, then serve. It returns when
// ctx ends, once the upkeep has stopped, or with the error that stopped the takeover.
func takeOver(ctx context.Context, work leaderWork, status *cluster.Status, log *slog.Logger) error {
	if err := work.Restore(ctx); err != nil {
		return err
	}
	if err := work.Settle(ctx); err != nil {
		return err
	}
	var upkeep sync.WaitGroup
	upkeep.Go(func() { work.Run(ctx) })
	status.Serve()
	log.Info("serving A2A requests as the leader")
	<-ctx.Done()
	upkeep.Wait()
	return nil
}

// mux serves the A2A endpoint and the card through the router, and this replica's own health and metrics. /readyz
// answers 200 while ready says so, and 503 otherwise.
func mux(router http.Handler, ready func() bool, tel *telemetry.Telemetry) *http.ServeMux {
	m := http.NewServeMux()
	m.Handle(a2aPath, router)
	m.Handle(a2aPath+adapter.CardPath, router)
	// Alive once it listens: it stays cheap, and a takeover that takes long does not fail it.
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// Ready on a standby, which forwards, and on a leader once its takeover is done, until the replica starts to stop.
	// The store is open and migrated before the listener starts.
	m.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	m.Handle("/metrics", tel.Handler())
	return m
}

// openStore opens the store the configuration names.
func openStore(ctx context.Context, cfg config.Config, opts store.Options) (*store.Store, error) {
	if cfg.Store == settings.StorePostgres {
		return store.OpenPostgres(ctx, cfg.PostgresDSN, opts)
	}
	return store.OpenSQLite(ctx, cfg.SQLitePath, opts)
}
