package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/postgres"
)

// Run starts the broker and runs until ctx ends or something fails. It returns nil after a clean stop
// (ctx ended, even while it was still starting), and otherwise an error: one wrapping ErrWiring is a usage error, one wrapping
// k8ssecrets.ErrAccessDenied is missing RBAC, and any other is a failure of a dependency. cmd/courier
// maps them to exit codes.
//
// The order is klog (before anything uses client-go, so even its first line is capped and routed),
// the Kubernetes client, the health server (so that /healthz answers from here on), Bootstrap (the
// config and the Secret caches), the one ChatConnection, the store (SQLite, or Postgres with its DSN read from
// a Secret: cfg.Store), and then readiness: /readyz says 503 "starting" until the last of those is done. The
// connection comes before the store, so a config that v1 cannot serve leaves no database behind. That is what every
// replica runs, a standby included (spec 11).
//
// What only the leader runs follows, as leaderHalf.start has it: the audit writer, the Bindings, the chat platform,
// the outbox, the session manager and its recovery, the sweeps, the interactions and the chat connection. With SQLite
// it starts at once. With Postgres a replica first wins a Kubernetes Lease (cfg.Election) and is given the next epoch
// by the store, which fences every write of a replica that is no longer the leader, and its chat platform refuses every
// write once the lease is past its renew deadline (chatFence); a replica that loses the lease, meets a stale epoch or is
// refused a chat write ends the process at once (ExitLeadershipLost).
func Run(ctx context.Context, cfg RunConfig) (err error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	log := slog.New(telemetry.LogHandler{Handler: cfg.logger().Handler()})
	provider := cfg.TracerProvider
	if provider == nil {
		var shutdown func(context.Context) error
		// Before the exporter exists: it reads and parses its own environment variables as it is built.
		// With tracing off there is no exporter, and the process-wide hooks are left alone.
		if cfg.Telemetry.Endpoint != "" {
			telemetry.InstallDiagnostics(log)
		}
		provider, shutdown, err = cfg.Telemetry.Provider(ctx, cfg.ServiceVersion, log)
		if err != nil {
			return fmt.Errorf("telemetry: %w", err)
		}
		defer func() {
			flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			if ferr := shutdown(flush); ferr != nil {
				log.Warn("trace export flush failed", "error_type", telemetry.ErrorClass(ferr))
			}
		}()
	}
	defer func() { // a signal during startup makes some step fail with the context's error: a clean stop
		// A rejected token that was reported first stays a failure whatever signal follows it.
		if err != nil && ctx.Err() != nil && !errors.Is(err, ErrSlackAuth) {
			err = nil
		}
	}()
	if !cfg.leaveKlog {
		if err := RouteKlog(log, cfg.KlogVerbosity); err != nil {
			return fmt.Errorf("route klog: %w", err)
		}
	}
	client := cfg.Client
	if client == nil {
		if client, err = InClusterClient(); err != nil {
			return err
		}
	}
	// runCtx is the broker's own: a signal ends ctx, and the shutdown then stops the parts in order, one
	// step at a time, where a context derived from ctx would stop them all at once. Until startup has
	// finished, though, a signal does cancel it, so that a slow Bootstrap, Reconcile or Recover cannot
	// hold a signal for the length of the grace period.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopStartupWatch := context.AfterFunc(ctx, cancel)
	defer stopStartupWatch()
	defer cancel()
	sd := newShutdown(log)
	sd.trace = cfg.shutdownTrace
	// The shutdown waits for a leader half that is starting (aborting it), refuses one that would start later, runs the
	// steps in order, and only then lets go of the lease, which was renewed to the end: the other replica never starts
	// beside a leader that is still draining.
	gate := &startGate{}
	var cand *candidacy
	defer func() {
		gate.close(cancel)
		sd.run(runCtx)
		cand.stop(ctx)
	}()

	// replaced in 35f2b4 for the hooks; the stale gauge already reads the Config once Bootstrap has made it.
	var current atomic.Pointer[Config]
	// Stale until the first load: nothing is known to be fresh before it, and /readyz says "starting".
	metrics, err := NewMetrics(func() bool { c := current.Load(); return c == nil || c.Stale() })
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	obs, err := telemetry.New(metrics.reg, provider)
	if err != nil {
		return fmt.Errorf("telemetry metrics: %w", err)
	}
	runCtx = telemetry.WithObserver(runCtx, obs)
	ready := &Readiness{}
	served, servedDone := make(chan error, 1), make(chan struct{})
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	go func() { served <- Serve(serveCtx, cfg.Listener, NewHandler(metrics, ready)); close(servedDone) }()
	sd.add(stepHealth, func(c context.Context) { stopServing(); waitFor(c, servedDone) })

	var extraNamespaces []string // the Postgres DSN's Secret is courier's own, so no resource of the config names its namespace
	if cfg.Store.kind() == StorePostgres {
		extraNamespaces = append(extraNamespaces, cfg.Store.PostgresDSN.Namespace)
	}
	boot, err := Bootstrap(runCtx, client, BootstrapOptions{
		Dir: cfg.ConfigDir, ExtraNamespaces: extraNamespaces, SyncTimeout: cfg.SyncTimeout, PollInterval: cfg.reloadPoll, Logger: log, OnReloadError: metrics.ReloadError,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	current.Store(boot)
	sd.add(stepResolver, func(context.Context) { cancel(); boot.Reloader.Wait(); boot.Stop() }) // the Reloader stops before the resolver

	conn, err := ChooseConnection(runCtx, boot.Reloader.Current(), boot.Secrets)
	if err != nil {
		return err
	}
	connName := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	log.InfoContext(ctx, "serving a ChatConnection", "connection", connName)

	// A write that finds another replica has led since ends the process (spec D37): the error alone would be handled
	// by whichever part made the write, and the part could carry on. SQLite never calls it.
	fail := &failStop{log: log, exit: cfg.exitFunc()}
	st, err := openStore(runCtx, cfg.Store, cfg.DBPath, boot.Secrets, log,
		postgres.WithOnDeposed(func() { fail.now("a write found its epoch stale: another replica has led since") }))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if cfg.wrapStore != nil {
		st = cfg.wrapStore(st)
	}
	log.InfoContext(ctx, "opened the store", "store", cfg.Store.kind()) // the kind only: a DSN is never logged
	if err := st.RegisterMetrics(metrics.reg); err != nil {
		_ = st.Close()
		return fmt.Errorf("store metrics: %w", err)
	}
	sd.add(stepStore, func(context.Context) {
		if err := st.Close(); err != nil {
			log.Warn("closing the store", "error", err)
		}
	})
	// A store that fences (Postgres) means more than one replica may run, and one of them leads: the Lease picks it.
	// SQLite is one replica's, which leads at once and has no Lease.
	fenced, _ := st.(store.Fenced)
	if fenced != nil {
		if err := cfg.Election.validate(); err != nil {
			return err
		}
	}

	// Everything that every replica runs is up. A standby is as ready as a leader: readiness never depends on the
	// lease, since nothing routes traffic to courier in Socket Mode, and a standby is healthy (spec 11).
	ready.Set(append(boot.Checks(), StoreOpen(st))...)
	log.InfoContext(ctx, "ready")

	// The leader half starts here with SQLite, and with a fencing store when the lease is won and the store has its
	// epoch. Whatever ends it, or fails to start it, is sent to leaderDone.
	// Besides the store's epoch, a leader's writes to the chat are fenced by its own clock: once its lease is past the renew
	// deadline it writes nothing, and ends (chatFence). Only a replica with a Lease has one to be past.
	var lease *leaseClock
	var guard *chatFence
	if fenced != nil {
		lease = newLeaseClock(cfg.leaseNow)
		guard = newChatFence(cfg.Election, lease, fail.now)
	}
	leader := &leaderHalf{cfg: cfg, log: log, sd: sd, metrics: metrics, boot: boot, st: st, chatFence: guard}
	leaderDone := make(chan error, 1)
	runLeader := func() {
		leave, ok := gate.enter()
		if !ok {
			return // a shutdown has begun
		}
		if fenced != nil {
			epoch, err := takeLeadership(runCtx, fenced, cfg.Election, log)
			if err != nil {
				leave()
				leaderDone <- err
				return
			}
			metrics.Takeover()
			log.InfoContext(ctx, "leading", "epoch", epoch, "holder", cfg.Election.Identity)
		}
		// The connection and its Secrets as they are now: the startup read above only checks that the config can be served.
		// Everything else the leader half uses (the Bindings, the config snapshot, the Secrets) it reads live.
		current, err := ChooseConnection(runCtx, boot.Reloader.Current(), boot.Secrets)
		if err != nil {
			leave()
			leaderDone <- err
			return
		}
		metrics.Leader(true)
		chatDone, err := leader.start(ctx, runCtx, current)
		leave()
		if err != nil {
			leaderDone <- err
			return
		}
		leaderDone <- <-chatDone
	}
	if fenced == nil {
		go runLeader()
	} else {
		cand, err = startCandidacy(ctx, client, cfg.Election, log, lease, candidacyCallbacks{
			gained: func(context.Context) { runLeader() },
			lost:   func() { fail.now(leaseLostReason) },
		})
		if err != nil {
			return err
		}
	}

	// From here a signal ends ctx only, and the shutdown stops the parts in order. A signal during the leader half's
	// start also aborts it (the shutdown does, through the gate), so that a Recover or Reconcile cannot hold a signal
	// for the length of the grace period.
	stopStartupWatch()
	select {
	case <-ctx.Done():
		return nil
	case err := <-leaderDone:
		if ctx.Err() != nil && !errors.Is(err, ErrSlackAuth) { // stopped by the signal, not by Slack
			return nil
		}
		return err
	case err := <-served:
		served <- err // for the cleanup, which waits for it too
		return fmt.Errorf("the health server stopped before the broker did: %w", err)
	}
}

// takeLeadership gives the store its epoch, trying again until the time the leader is allowed to renew its lease has
// passed: past that the lease is as good as lost, and the replica ends and lets the other one try.
func takeLeadership(ctx context.Context, f store.Fenced, cfg ElectionConfig, log *slog.Logger) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.withDefaults().RenewDeadline)
	defer cancel()
	epoch, err := takeEpoch(ctx, f, cfg.Identity, takeRetryEvery, log)
	if err != nil {
		return 0, fmt.Errorf("take the leader's epoch: %w", err)
	}
	return epoch, nil
}

// renderSink is what Run needs of the renderer, which is *live.Sink: the session manager's Sink, and
// the notice hook of the chat intake, whose error leaves a notice's event unacknowledged so that it is
// redelivered.
type renderSink interface {
	session.Sink
	QueueNotice(ctx context.Context, key session.Key, n session.Notice) error
}
