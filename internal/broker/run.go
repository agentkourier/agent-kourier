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
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// Run starts the broker and runs until ctx ends or something fails. It returns nil after a clean stop
// (ctx ended, even while it was still starting), and otherwise an error: one wrapping ErrWiring is a usage error, one wrapping
// k8ssecrets.ErrAccessDenied is missing RBAC, and any other is a failure of a dependency. cmd/courier
// maps them to exit codes.
//
// The order is klog (before anything uses client-go, so even its first line is capped and routed),
// the Kubernetes client, the health server (so that /healthz answers from here on), Bootstrap (the
// config and the Secret caches), the one ChatConnection, the store, the audit writer, the Bindings,
// and then readiness: /readyz says 503 "starting" until the last of those is done. The connection
// comes before the store, so a config that v1 cannot serve leaves no database behind.
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
	if err := RouteKlog(log, cfg.KlogVerbosity); err != nil {
		return fmt.Errorf("route klog: %w", err)
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
	defer func() { sd.run(runCtx) }()

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

	boot, err := Bootstrap(runCtx, client, BootstrapOptions{
		Dir: cfg.ConfigDir, SyncTimeout: cfg.SyncTimeout, PollInterval: cfg.reloadPoll, Logger: log, OnReloadError: metrics.ReloadError,
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

	st, err := sqlite.Open(runCtx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := st.RegisterMetrics(metrics.reg); err != nil {
		_ = st.Close()
		return fmt.Errorf("store metrics: %w", err)
	}
	sd.add(stepStore, func(context.Context) {
		if err := st.Close(); err != nil {
			log.Warn("closing the store", "error", err)
		}
	})
	var auditLog store.Audit = st
	if cfg.auditStore != nil {
		auditLog = cfg.auditStore(st)
	}
	auditor := audit.New(auditLog, audit.Options{Logger: log, OnFailure: metrics.AuditFailure})
	bindings := NewBindings(boot.Reloader, boot.Secrets, auditor, Options{OnRejected: metrics.CredentialRejected})
	sd.add(stepPool, func(context.Context) { bindings.Close() })

	ready.Set(append(boot.Checks(), StoreOpen(st))...)
	log.InfoContext(ctx, "ready")

	// The chat platform, built for the connection's ChatConnection.platform: it posts for the outbox, and a token revoked
	// mid-run, which only a post sees, ends the run with ErrSlackAuth (WatchAuth, WithAuthFailure).
	// courier_chat_connected says 0 from here, and 1 while the connection is up. /readyz does not hold it: see NewHandler.
	metrics.ChatConnected(connName, false)
	platform, err := cfg.newPlatform(conn, log, func(up bool) { metrics.ChatConnected(connName, up) })
	if err != nil {
		return err
	}
	chatCtx, report, stopChat := WithAuthFailure(runCtx)
	ob := outbox.New(outbox.Config{
		Store: st, Platform: WatchAuth(platform, report), Logger: log, OnAbandon: metrics.OutboxAbandoned,
	})
	if err := ob.Reconcile(runCtx); err != nil { // Slack may be down: Run below tries again, so this only says so
		log.WarnContext(ctx, "outbox: the first reconcile did not finish; it will be tried again", "error", err)
	}
	outboxCtx, stopOutbox := context.WithCancel(runCtx)
	outboxDone := make(chan struct{})
	go func() { defer close(outboxDone); _ = ob.Run(outboxCtx) }()
	sd.add(stepDrain, func(c context.Context) { // stop the drain loop, then post what is pending one last time
		stopOutbox()
		waitFor(c, outboxDone)
		if err := ob.Reconcile(c); err != nil && c.Err() == nil {
			log.Warn("outbox: the final drain did not finish", "error", err)
		}
		// Slack's connections close only now: the drain posts through them, and a connection that its
		// calls opened after an earlier close would be one that nothing closes.
		platform.closeIdle()
	})

	// The renderer posts a turn's first message through the outbox and edits or streams it through the
	// chat platform (the Slack adapter over the client); a notice is only queued on the outbox, off Slack's
	// acknowledgement path.
	renderer := live.New(live.Config{Sender: ob, Platform: platform, Store: st, Logger: log})
	var sink renderSink = live.NewSink(renderer, ob, log)
	if cfg.sink != nil {
		sink = cfg.sink
	}
	// The manager's sink is the tap, which hands the pauses of its turns to the interactions' intake; the
	// intake answers through the manager, so its session is bound once the manager is built (D2).
	// Whatever the interactions start adds its stop to stops, which runs between the intake, which feeds
	// them, and the manager, which they feed.
	stops := stopList{trace: cfg.stopTrace}
	sd.add(stepInteractions, stops.run)
	parts := interactionDeps{st: st, ob: ob, platform: platform, auditor: auditor, bindings: bindings, metrics: metrics, log: log, trace: cfg.startTrace}
	tap, late, err := parts.newTap(sink)
	if err != nil {
		return err
	}
	mgr, err := session.New(session.Config{Context: runCtx, Store: st, Bindings: bindings, Users: users{platform}, Sink: tap, Logger: log, Audit: auditor, OnTurnEnd: metrics.TurnOutcome})
	if err != nil {
		return fmt.Errorf("session manager: %w", err)
	}
	late.bind(mgr)
	sd.add(stepManager, func(c context.Context) {
		if err := mgr.Close(c); err != nil {
			log.Warn("closing the session manager", "error", err)
		}
		// The renders that were writing a final text when their turns ended are waited for here, after the
		// turns, and before the drain that stops the outbox they post through.
		if err := renderer.Wait(c); err != nil {
			log.Warn("waiting for the renders of the last turns", "error", err)
		}
	})
	if err := parts.recovered("recover-sessions", func() error { return mgr.Recover(runCtx) }); err != nil {
		return fmt.Errorf("recover sessions: %w", err)
	}
	interactions, err := parts.start(runCtx, mgr, tap, &stops)
	if err != nil {
		return err
	}

	// A match of a botMessage trigger that starts a turn becomes a session through the manager's intake, the path a
	// mention takes. The threads of the keys are in the store; the permalink comes from what the platform learned when it
	// connected, since the start runs on the acknowledgement path and may call neither Slack nor the agent.
	starter := &triggerStarter{conn: connName, submit: mgr, notice: sink.QueueNotice, audit: auditor, link: permalinks(platform.Platform), log: log}
	intake := chat.NewIntake(chat.IntakeConfig{
		Connection: conn.Ref, Snapshot: boot.Reloader.Current, Submitter: mgr, Interactions: interactions, Logger: log,
		Notice: sink.QueueNotice, TriggerThreads: st, OnStart: starter.OnStart,
	})
	chatDone, chatStopped := make(chan error, 1), make(chan struct{})
	parts.traced("open-intake")
	go func() {
		chatDone <- RunPlatform(chatCtx, platform, intake, RunPolicy{Logger: log})
		close(chatStopped)
	}()
	sd.add(stepIntake, func(c context.Context) { // the intake stops before the interactions and the manager it feeds
		stopChat()
		waitFor(c, chatStopped)
	})

	// From here a signal ends ctx only, and the shutdown stops the parts in order. A signal between
	// ready.Set and this line still cancels everything at once: that window is accepted, because it is
	// still startup, and the alternative is a Recover or Reconcile that ignores a signal and spends the grace period.
	stopStartupWatch()
	select {
	case <-ctx.Done():
		return nil
	case err := <-chatDone:
		chatDone <- err
		if ctx.Err() != nil && !errors.Is(err, ErrSlackAuth) { // stopped by the signal, not by Slack
			return nil
		}
		return err
	case err := <-served:
		served <- err // for the cleanup, which waits for it too
		return fmt.Errorf("the health server stopped before the broker did: %w", err)
	}
}

// renderSink is what Run needs of the renderer, which is *live.Sink: the session manager's Sink, and
// the notice hook of the chat intake, whose error leaves a notice's event unacknowledged so that it is
// redelivered.
type renderSink interface {
	session.Sink
	QueueNotice(ctx context.Context, key session.Key, n session.Notice) error
}
