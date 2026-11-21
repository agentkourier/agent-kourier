// Command adapter serves a Claude Managed Agents agent as an A2A agent.
//
// It is configured through its environment (package config holds every variable and its check; package telemetry the
// OTEL_* ones) and reads credentials as the Anthropic Go SDK does (ANTHROPIC_API_KEY, or the active `ant auth login`
// profile). A setting it cannot use stops it at start, with an error that names the variable. It takes one flag,
// --version.
//
// With SQLite it is the only replica and leads at once. With Postgres its replicas elect a leader through the store
// (package cluster). The leader restores each A2A context's session from the store and reconciles it with the agent's
// unarchived sessions in CMA, fails the tasks a restart or a takeover cut off, starts the store's upkeep, and only then
// serves A2A requests; a standby forwards them to it. A2A requests must carry one of the tokens in AUTH_TOKEN_FILE;
// the card, /healthz, /readyz and /metrics are open.
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
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
	"github.com/agentkourier/agent-kourier/addons/cma/auth"
	"github.com/agentkourier/agent-kourier/addons/cma/cluster"
	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
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
		if traces, err = telemetry.LoadConfig(getenv); err == nil {
			log := slog.New(telemetry.NewLogHandler(stderr, cfg.LogLevel))
			slog.SetDefault(log)
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if err := serve(ctx, cfg, traces, log); err != nil {
				log.Error("adapter stopped", "error", err)
				return exitFailure
			}
			return exitOK
		}
	}
	slog.New(telemetry.NewLogHandler(stderr, slog.LevelInfo)).Error("configuration", "error", err)
	return exitUsage
}

// serve runs the adapter until ctx ends or a dependency fails.
func serve(ctx context.Context, cfg config.Config, traces telemetry.Config, log *slog.Logger) error {
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
	tel, err := telemetry.New(provider)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	var tokens *auth.Tokens
	if !cfg.AuthDisabled {
		if tokens, err = auth.LoadTokens(cfg.AuthTokenFile, log); err != nil {
			return fmt.Errorf("AUTH_TOKEN_FILE: %w", err)
		}
	}

	lost := &failStop{log: log, exit: os.Exit}
	st, err := openStore(ctx, cfg, store.Options{Log: log, OnDeposed: func() { lost.now("a write found that another replica has taken the lease") }})
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
	if err := tel.ObserveSessions(exec.Contexts); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	rpc := a2asrv.NewJSONRPCHandler(adapter.NewRequestHandler(exec, a2asrv.WithCallInterceptors(auth.Interceptor{})))
	if tokens != nil {
		go tokens.Watch(ctx, auth.ReloadInterval)
		rpc = (&auth.Check{Header: cfg.AuthHeader, Tokens: tokens, Log: log,
			Refused: func(reason string) { tel.AuthRefused(telemetry.AuthReason(reason)) }}).Wrap(rpc)
	} else {
		log.Warn("the caller check is off (AUTH_DISABLED=true): anything that reaches the loopback address can run the agent", "addr", cfg.ListenAddr)
	}
	card := adapter.Card("claude-managed-agent", "A Claude Managed Agents agent served over A2A", cfg.PublicURL, version)
	status := &cluster.Status{}
	router := &cluster.Router{Local: telemetry.Extract(adapter.Mux(a2aPath, card, rpc)), Status: status,
		RetryAfter: cfg.Lease.RetryPeriod, Telemetry: tel, Log: log}
	var elector *cluster.Elector
	if cfg.Store == config.StorePostgres {
		elector = &cluster.Elector{Lease: st, Holder: cfg.PodName, Address: cfg.AdvertiseURL, LeaseDuration: cfg.Lease.Duration,
			RenewDeadline: cfg.Lease.RenewDeadline, RetryPeriod: cfg.Lease.RetryPeriod, Status: status, Telemetry: tel, Log: log}
		router.Leaders = elector
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	srv := &http.Server{Handler: mux(router, status, tel), ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("adapter listening", "version", version, "addr", cfg.ListenAddr, "card", cfg.PublicURL+adapter.CardPath,
		"agent", cfg.AgentID, "environment", cfg.EnvironmentID, "store", cfg.Store, "auth_header", cfg.AuthHeader,
		"auth_disabled", cfg.AuthDisabled, "session_idle_ttl", cfg.SessionIdleTTL, "tool_result_timeout", cfg.ToolResultTimeout,
		"task_retention", cfg.TaskRetention, "pod", cfg.PodName, "traces", traces.Endpoint != "")

	// takeOver is the leader's work: reconcile, fail the interrupted turns, start the upkeep, then serve. It returns
	// when ctx ends, or with the error that stopped the takeover.
	takeOver := func(ctx context.Context) error {
		if err := exec.Restore(ctx); err != nil {
			return err
		}
		if err := exec.Settle(ctx); err != nil {
			return err
		}
		go exec.Run(ctx)
		status.Serve()
		log.Info("serving A2A requests as the leader")
		<-ctx.Done()
		return nil
	}
	// The election outlives ctx until the server has stopped, so that the lease is renewed while requests drain and a
	// standby does not take over while this replica still writes.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()
	led := make(chan error, 1)
	go func() {
		if elector == nil {
			status.Lead()
			tel.SetLeader(true)
			led <- takeOver(electionCtx)
			return
		}
		led <- elector.Run(electionCtx, takeOver)
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
			lost.now("the lease was lost or could not be renewed")
		}
		failure = err
	}
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("stop serving", "error", err)
	}
	stopElection()
	if !ledDone {
		// The election's context has ended, so Run, or the takeover, returns at once; a takeover it cut short says so.
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

// mux serves the A2A endpoint and the card through the router, and this replica's own health and metrics.
func mux(router http.Handler, status *cluster.Status, tel *telemetry.Telemetry) *http.ServeMux {
	m := http.NewServeMux()
	m.Handle(a2aPath, router)
	m.Handle(a2aPath+adapter.CardPath, router)
	// Alive once it listens: it stays cheap, and a takeover that takes long does not fail it.
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// Ready on a standby, which forwards, and on a leader once its takeover is done. The store is open and migrated
	// before the listener starts.
	m.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !status.Ready() {
			http.Error(w, "taking over as the leader", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	m.Handle("/metrics", tel.Handler())
	return m
}

// failStop ends the process at once, once, with a line that says why: the answer to a lost lease and to a write the
// store refused as deposed. A drain would let a replica that no longer leads keep running turns.
type failStop struct {
	log  *slog.Logger
	exit func(code int)
	once sync.Once
}

func (f *failStop) now(reason string) {
	f.once.Do(func() {
		f.log.Error("leadership lost: stopping at once, without a drain", "reason", reason)
		f.exit(cluster.ExitLeadershipLost)
	})
}

// openStore opens the store the configuration names.
func openStore(ctx context.Context, cfg config.Config, opts store.Options) (*store.Store, error) {
	if cfg.Store == config.StorePostgres {
		return store.OpenPostgres(ctx, cfg.PostgresDSN, opts)
	}
	return store.OpenSQLite(ctx, cfg.SQLitePath, opts)
}
