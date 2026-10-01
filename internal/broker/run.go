package broker

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
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
// (ctx ended), and otherwise an error: one wrapping ErrWiring is a usage error, one wrapping
// k8ssecrets.ErrAccessDenied is missing RBAC, and any other is a failure of a dependency. cmd/courier
// maps them to exit codes.
//
// The order is klog (before anything uses client-go, so even its first line is capped and routed),
// the Kubernetes client, the health server (so that /healthz answers from here on), Bootstrap (the
// config and the Secret caches), the one ChatConnection, the store, the audit writer, the Bindings,
// and then readiness: /readyz says 503 "starting" until the last of those is done. The connection
// comes before the store, so a config that v1 cannot serve leaves no database behind.
func Run(ctx context.Context, cfg RunConfig) (err error) {
	cfg.WarnTestOptions()
	if err := cfg.validate(); err != nil {
		return err
	}
	log := cfg.logger()
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
	metrics, err := NewMetrics(func() bool { c := current.Load(); return c != nil && c.Stale() })
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

	select {
	case <-ctx.Done():
		return nil
	case err := <-served:
		served <- err // for the cleanup, which waits for it too
		return fmt.Errorf("the health server stopped before the broker did: %w", err)
	}
}
