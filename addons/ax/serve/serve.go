// Package serve runs the agent from its settings: the caller check, the store, the leader election, the settlement
// of the turns a restart or a takeover cut off, the A2A endpoint and its card, /healthz, /readyz and /metrics, and the
// store's upkeep, until its context ends.
//
// With SQLite the agent is the only replica and leads at once. With Postgres its replicas elect a leader through the
// store (package cluster). The leader fails the tasks a restart or a takeover cut off, starts the store's upkeep, and
// only then serves A2A requests; a standby forwards them, and the card, to it, and settles and deletes nothing. A leader
// whose lease another replica takes, or that cannot renew it within LEASE_RENEW_DEADLINE, ends the process at once with
// cluster.ExitLeadershipLost; one that stops on its context hands the lease over once its requests have drained.
//
// Standalone (RUNNER_URL empty), the agent answers /readyz itself: 200 on a standby, which forwards, and on a leader
// once its takeover is done, until it starts to shut down. Beside the AX task runner (RUNNER_URL set), /readyz and
// /metadata/ are the runner's, proxied to it. /metrics is open, as /healthz and /readyz are.
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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/auth"
	"github.com/agentkourier/agent-kourier/addons/ax/cluster"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// ShutdownTimeout bounds the wait for the requests in flight at a graceful stop (and the command's flush of its spans
// after it), and releaseTimeout the hand-over of the lease after the requests. Together they stay well inside
// Kubernetes's default grace period of 30 s.
const (
	ShutdownTimeout = 5 * time.Second
	releaseTimeout  = 3 * time.Second
)

// readHeaderTimeout bounds how long a caller may take to send a request's headers before the server closes the
// connection.
const readHeaderTimeout = 10 * time.Second

// Options are what Run takes beside the settings and the model.
type Options struct {
	// Version is the agent's, which the card names and the log reports.
	Version string
	// Log receives the agent's log. Nil is slog.Default().
	Log *slog.Logger
	// Telemetry is the agent's metrics, which /metrics serves, and its tracer: built from agent.TelemetrySpec, and Run
	// adds the agent's own metrics to it (newUpkeep). Nil is a Telemetry of Run's own that records no spans.
	Telemetry *telemetry.Telemetry
	// Routes, if set, adds routes to the server's mux before it serves (the e2e agent's test controls).
	Routes func(*http.ServeMux)
	// Listening, if set, is told the address the agent listens on, once it does: before its takeover, if it leads, is
	// done.
	Listening func(net.Addr)
	// Exit ends the process with cluster.ExitLeadershipLost when this replica stops leading while it runs. Nil is
	// os.Exit. If it returns (a test's), Run goes on until the election finds the lease lost, then stops as on a
	// failure and returns cluster.ErrLeadershipLost.
	Exit func(code int)
}

// Run serves the agent that cfg describes, with m as its model, until ctx ends (it then returns nil, after a graceful
// stop and, if it leads, the release of the lease) or a dependency fails.
func Run(ctx context.Context, cfg config.Config, m model.LLM, opts Options) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	tel := opts.Telemetry
	if tel == nil {
		var err error
		if tel, err = telemetry.New(agent.TelemetrySpec(), nil); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
	}
	exit := opts.Exit
	if exit == nil {
		exit = os.Exit
	}
	check, watch, err := callerCheck(cfg, log, tel)
	if err != nil {
		return err
	}
	lost := &failStop{log: log, exit: exit}
	st, err := openStore(ctx, cfg, store.Options{Log: log, OnDeposed: func() { lost.now("a write found that another replica has taken the lease") }})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	upkeep, err := newUpkeep(cfg, st, log, tel)
	if err != nil {
		return err
	}
	srv, err := agent.New(agent.Config{Model: m, Store: st, Check: check, PublicURL: cfg.PublicURL, Version: opts.Version, Telemetry: tel})
	if err != nil {
		return err
	}
	status := &cluster.Status{}
	router := &cluster.Router{Local: telemetry.Extract(srv.Handler), Status: status, RetryAfter: cfg.Lease.RetryPeriod, Telemetry: tel, Log: log}
	var stopping atomic.Bool
	mux, err := Mux(router, cfg.RunnerURL, func() bool { return !stopping.Load() && status.Ready() })
	if err != nil {
		return err
	}
	mux.Handle("/metrics", tel.Handler())
	if opts.Routes != nil {
		opts.Routes(mux)
	}

	var elector *cluster.Elector
	if cfg.Store == config.StorePostgres {
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
	release := func() {
		if elector == nil {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := elector.Release(releaseCtx); err != nil {
			log.Warn("hand the lease over; a standby takes it once it expires", "error", err)
		}
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		release()
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()
	// The background work ends when Run returns, whether or not ctx has ended, and before the store closes.
	var background sync.WaitGroup
	defer background.Wait()
	backgroundCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	if watch != nil {
		background.Go(func() { watch(backgroundCtx) })
	}
	if opts.Listening != nil {
		opts.Listening(ln.Addr())
	}
	log.Info("agent listening", "version", opts.Version, "addr", ln.Addr().String(), "card", cfg.PublicURL+agent.CardPath,
		"model", cfg.ModelName, "region", cfg.ModelRegion, "store", cfg.Store, "pod", cfg.PodName, "auth", authMode(cfg),
		"auth_header", cfg.AuthHeader, "runner", !cfg.Standalone(), "task_retention", cfg.TaskRetention, "session_idle_ttl", cfg.SessionIdleTTL)

	// takeOver is the leader's work, once it holds the lease: fail the turns a restart or a takeover cut off, start the
	// upkeep, then serve. On Postgres no other replica serves turns from the store by then: the lease's epoch fences
	// every write of the one that held it. It returns when ctx ends, once the upkeep has stopped, or with the error
	// that stopped the takeover.
	takeOver := func(ctx context.Context) error {
		n, err := st.SettleInterrupted(ctx)
		if err != nil {
			return err
		}
		tel.TasksInterrupted(n)
		if n > 0 {
			log.Info("failed the turns a restart or a takeover cut off", "tasks", n)
		}
		var swept sync.WaitGroup
		swept.Go(func() { upkeep.Run(ctx) })
		status.Serve()
		log.Info("serving A2A requests as the leader")
		<-ctx.Done()
		swept.Wait()
		return nil
	}
	// The election outlives ctx until the server has stopped, so that the lease is renewed while requests drain and a
	// standby does not take over while this replica still writes.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()
	led := make(chan error, 1)
	go func() {
		if elector == nil {
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
	stopping.Store(true)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(drain); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	release()
	return failure
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

// Mux serves a2a, the A2A endpoint and its card, with /healthz and /readyz. With runnerURL set, /readyz and /metadata/
// are proxied to the AX task runner there; without it, /readyz answers 200 while ready says so and 503 otherwise.
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

// newUpkeep builds the leader's upkeep of st, as cfg sets it, and registers in tel the metric of what it deletes
// (agent.NewMetrics).
func newUpkeep(cfg config.Config, st agent.UpkeepStore, log *slog.Logger, tel *telemetry.Telemetry) (*agent.Upkeep, error) {
	metrics, err := agent.NewMetrics(tel)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return &agent.Upkeep{Store: st, TaskRetention: cfg.TaskRetention, SessionIdleTTL: cfg.SessionIdleTTL, Log: log, Metrics: metrics}, nil
}

// callerCheck builds the check cfg names, which counts each refusal in tel, and for the token file the watch that keeps
// its tokens current.
func callerCheck(cfg config.Config, log *slog.Logger, tel *telemetry.Telemetry) (*auth.Check, func(context.Context), error) {
	refused := tel.AuthRefused
	switch {
	case cfg.AuthDisabled:
		log.Warn("the caller check is off (AUTH_DISABLED=true): anything that reaches the loopback address can run the agent", "addr", cfg.ListenAddr)
		return &auth.Check{Verifier: auth.Open{}, Log: log, Refused: refused}, nil, nil
	case cfg.OIDC():
		v, err := auth.NewOIDC(cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCSubject)
		if err != nil {
			return nil, nil, fmt.Errorf("OIDC: %w", err)
		}
		return &auth.Check{Verifier: v, Bearer: true, Log: log, Refused: refused}, nil, nil
	default:
		tokens, err := auth.LoadTokens(cfg.AuthTokenFile, log)
		if err != nil {
			return nil, nil, fmt.Errorf("AUTH_TOKEN_FILE: %w", err)
		}
		check := &auth.Check{Verifier: auth.TokenVerifier{Header: cfg.AuthHeader, Tokens: tokens},
			Bearer: strings.EqualFold(cfg.AuthHeader, "Authorization"), Log: log, Refused: refused}
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
