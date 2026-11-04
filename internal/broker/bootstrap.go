package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// defaultSyncTimeout bounds how long Bootstrap waits for the Secret caches.
const defaultSyncTimeout = 30 * time.Second

// BootstrapOptions configures Bootstrap.
type BootstrapOptions struct {
	// Dir is the config directory.
	Dir string
	// ExtraSecrets are Secrets that are followed, from the start to the end of the process, although no resource of the
	// config refers to one: the Postgres DSN's Secret, which belongs to Agent Kourier and not to a resource.
	ExtraSecrets []config.SecretName
	// SyncTimeout bounds the wait for the Secret caches to sync; zero means 30 s.
	SyncTimeout time.Duration
	// PollInterval, SecretsInterval and SecretsWait are the reloader's; zero means its defaults. SecretsWait bounds the
	// wait of each later reload for the Secrets it adds, which is not SyncTimeout's: that bounds the first.
	PollInterval, SecretsInterval, SecretsWait time.Duration
	// OnReloadError is called for each failed reload, for a metric; see config.ReloaderConfig.
	OnReloadError func(error)
	// Logger receives the reloader's and the resolver's reports. It defaults to slog.Default().
	Logger *slog.Logger
}

// Config is the loaded config and the Secrets it reads: a Reloader whose snapshots are checked
// against a resolver that follows the Secrets the config names.
type Config struct {
	// Reloader holds the current snapshot, and is the Snapshots that Bindings reads.
	Reloader *config.Reloader
	// Secrets is the resolver the Reloader checks against, and the identity package reads through.
	Secrets *k8ssecrets.Resolver
}

// Stale reports whether the latest reload failed, so the current snapshot lags the config on
// disk. A reload that refers to a Secret that Agent Kourier may not read, or that does not sync
// in time, fails this way and keeps the last good snapshot, until a later reload succeeds.
func (c *Config) Stale() bool { return c.Reloader.Status().Stale }

// Checks are the readiness conditions that Config holds: the first load and the synced caches.
func (c *Config) Checks() []Check {
	return []Check{ConfigLoaded(c.Reloader), SecretsSynced(c.Secrets)}
}

// Stop stops the Secret informers. The reloader stops with the context given to Bootstrap, so
// shut down in this order: cancel that context, wait for the reloader (Reloader.Wait), then Stop.
// Stopped the other way round, the reloader's next check finds every Secret unreadable, fails the
// reload and calls OnReloadError, which a metric would count as a real failure.
func (c *Config) Stop() { c.Secrets.Stop() }

// Bootstrap loads the config once to learn which Secrets it refers to, starts a resolver that
// follows those and opts.ExtraSecrets and waits for their caches (an error names the cause, so a
// denied list fails at once with "forbidden", wrapping k8ssecrets.ErrAccessDenied), then starts
// the Reloader over it, which follows the Secrets of each later config. The Reloader runs until
// ctx ends. On failure nothing is left running. A config that does not load because the operator
// must fix it (config.ErrInvalid) gives an error wrapping ErrWiring, since a restart would find it again.
func Bootstrap(ctx context.Context, client kubernetes.Interface, opts BootstrapOptions) (*Config, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	set, err := config.Load(ctx, opts.Dir)
	if err != nil {
		return nil, wiring(fmt.Errorf("load config: %w", err))
	}
	secrets := k8ssecrets.New(client, k8ssecrets.WithLogger(log), k8ssecrets.WithPinned(opts.ExtraSecrets...))
	syncCtx, cancel := context.WithTimeout(ctx, cmp.Or(opts.SyncTimeout, defaultSyncTimeout))
	defer cancel()
	// One bound for both waits, so Secrets that never sync cost the sync timeout once.
	if err := secrets.Start(syncCtx); err != nil {
		secrets.Stop()
		return nil, fmt.Errorf("start the secret resolver: %w", err)
	}
	if err := secrets.Watch(syncCtx, set.SecretNames()); err != nil {
		secrets.Stop()
		return nil, fmt.Errorf("start the secret resolver: %w", err)
	}
	rl := config.NewReloader(config.ReloaderConfig{
		Dir: opts.Dir, Secrets: secrets, PollInterval: opts.PollInterval, SecretsInterval: opts.SecretsInterval, SecretsWait: opts.SecretsWait,
		Logger: log, OnError: opts.OnReloadError,
	})
	if err := rl.Start(ctx); err != nil {
		secrets.Stop()
		return nil, wiring(err)
	}
	return &Config{Reloader: rl, Secrets: secrets}, nil
}

// wiring wraps ErrWiring around err when the config is at fault, and returns other errors as they are.
func wiring(err error) error {
	if errors.Is(err, config.ErrInvalid) {
		return fmt.Errorf("%w: %w", ErrWiring, err)
	}
	return err
}
