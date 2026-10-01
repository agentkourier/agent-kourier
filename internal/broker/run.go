package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// cleanups are the things Run must undo, run last registered first. That order is only what holds
// until WP-35f2c, which owns the shutdown order (intake, manager, outbox drain, pool, resolver, store)
// and replaces this stack with it: nothing may depend on the order here.
type cleanups struct{ fns []func() }

// add registers fn to run when run is called.
func (c *cleanups) add(fn func()) { c.fns = append(c.fns, fn) }

// run calls every registered function, last first, and forgets them, so a second run does nothing.
func (c *cleanups) run() {
	for len(c.fns) > 0 {
		fn := c.fns[len(c.fns)-1]
		c.fns = c.fns[:len(c.fns)-1]
		fn()
	}
}

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
	cfg.logTestOptions()
	if err := cfg.validate(); err != nil {
		return err
	}
	log := cfg.logger()
	defer func() { // a signal during startup makes some step fail with the context's error: a clean stop
		if err != nil && ctx.Err() != nil {
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
	runCtx, cancel := context.WithCancel(ctx)
	var undo cleanups
	defer undo.run()
	defer cancel()

	// replaced in 35f2b4 for the hooks; the stale gauge already reads the Config once Bootstrap has made it.
	var current atomic.Pointer[Config]
	// Stale until the first load: nothing is known to be fresh before it, and /readyz says "starting".
	metrics, err := NewMetrics(func() bool { c := current.Load(); return c == nil || c.Stale() })
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	ready := &Readiness{}
	served := make(chan error, 1)
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	go func() { served <- Serve(serveCtx, cfg.Listener, NewHandler(metrics, ready)) }()
	undo.add(func() { stopServing(); <-served }) // registered first, so it is the last to go: /healthz stays up

	boot, err := Bootstrap(runCtx, client, BootstrapOptions{Dir: cfg.ConfigDir, SyncTimeout: cfg.SyncTimeout, Logger: log})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	current.Store(boot)
	undo.add(func() { cancel(); boot.Reloader.Wait(); boot.Stop() }) // the Reloader stops before the resolver

	conn, err := ChooseConnection(runCtx, boot.Reloader.Current(), boot.Secrets)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "serving a ChatConnection", "connection", conn.Ref.Namespace+"/"+conn.Ref.Name)

	st, err := sqlite.Open(runCtx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	undo.add(func() {
		if err := st.Close(); err != nil {
			log.Warn("closing the store", "error", err)
		}
	})
	auditor := audit.New(st, audit.Options{Logger: log})
	bindings := NewBindings(boot.Reloader, boot.Secrets, auditor, Options{})
	undo.add(bindings.Close)

	ready.Set(append(boot.Checks(), StoreOpen(st))...)
	log.InfoContext(ctx, "ready")

	// Slack: its client posts for the outbox, and a token revoked mid-run, which only a post sees,
	// ends the Slack run with ErrSlackAuth (WatchPosts, WithAuthFailure).
	// The Slack client gets a transport of its own, so that its idle connections can be closed at
	// shutdown and no goroutine outlives Run.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	undo.add(transport.CloseIdleConnections)
	sc := slackConfig(conn, log)
	sc.HTTPClient = &http.Client{Transport: transport}
	var sl slackAPI
	if cfg.newSlack != nil {
		sl = cfg.newSlack(sc)
	} else {
		sl = slack.New(sc)
	}
	slackCtx, report, stopSlack := WithAuthFailure(runCtx)
	ob := outbox.New(outbox.Config{Store: st, Poster: WatchPosts(sl, report), Logger: log})
	if err := ob.Reconcile(runCtx); err != nil { // Slack may be down: Run below tries again, so this only says so
		log.WarnContext(ctx, "outbox: the first reconcile did not finish; it will be tried again", "error", err)
	}
	outboxCtx, stopOutbox := context.WithCancel(runCtx)
	outboxDone := make(chan struct{})
	go func() { defer close(outboxDone); _ = ob.Run(outboxCtx) }()
	undo.add(func() { stopOutbox(); <-outboxDone })

	mgr, err := session.New(session.Config{Store: st, Bindings: bindings, Users: sl, Sink: cfg.Sink, Logger: log, Audit: auditor})
	if err != nil {
		return fmt.Errorf("session manager: %w", err)
	}
	undo.add(func() {
		closeCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		if err := mgr.Close(closeCtx); err != nil {
			log.Warn("closing the session manager", "error", err)
		}
	})
	if err := mgr.Recover(runCtx); err != nil {
		return fmt.Errorf("recover sessions: %w", err)
	}

	intake := slack.NewIntake(slack.IntakeConfig{
		Connection: conn.Ref, Snapshot: boot.Reloader.Current, Submitter: mgr, Interactions: cfg.Interactions, Logger: log,
	})
	slackDone := make(chan error, 1)
	go func() { slackDone <- RunSlack(slackCtx, sl, intake, RunPolicy{Logger: log}) }()
	undo.add(func() { stopSlack(); <-slackDone }) // the intake stops before the manager it feeds

	select {
	case <-ctx.Done():
		return nil
	case err := <-slackDone:
		slackDone <- err
		if ctx.Err() != nil && !errors.Is(err, ErrSlackAuth) { // stopped by the signal, not by Slack
			return nil
		}
		return err
	case err := <-served:
		served <- err // for the cleanup, which waits for it too
		return fmt.Errorf("the health server stopped before the broker did: %w", err)
	}
}

// slackAPI is what Run uses of the Slack client, which is *slack.Client: the connection, the outbox's
// posts and the user lookups.
type slackAPI interface {
	SlackRunner
	outbox.Poster
	session.Users
}

// slackConfig is the Slack client's config for conn. Its API URL comes from testhooks.SlackAPIURL and
// from nothing else: that is "" in a production build, so the bot token can only be sent to Slack,
// and a test image sets it to reach the fake.
func slackConfig(conn SlackConnection, log *slog.Logger) slack.Config {
	cfg := slack.Config{BotToken: conn.BotToken, AppToken: conn.AppToken, Logger: log}
	if url := testhooks.SlackAPIURL(); url != "" {
		cfg.APIURL = url
	}
	return cfg
}
