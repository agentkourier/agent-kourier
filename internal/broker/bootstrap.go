package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
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
	// OnReport is called with the report of each published snapshot, for a metric; see config.ReloaderConfig.
	OnReport func(config.Report)
	// Logger receives the reloader's and the resolver's reports. It defaults to slog.Default().
	Logger *slog.Logger
	// CRDs, when set, makes Bootstrap read the custom resources of the cluster in place of Dir, which is then ignored.
	CRDs *CRDSource
}

// CRDSource says where the custom resources are read, and which ChatConnection this broker serves.
type CRDSource struct {
	// Client reads the three resources: InClusterDynamicClient's.
	Client dynamic.Interface
	// Connection is the one ChatConnection this broker serves (AGENTKOURIER_CHAT_CONNECTION).
	Connection v1alpha1.ObjectRef
}

// Config is the loaded config and the Secrets it reads: a Reloader whose snapshots are checked
// against a resolver that follows the Secrets the config names.
type Config struct {
	// Reloader holds the current snapshot, and is the Snapshots that Bindings reads.
	Reloader *config.Reloader
	// Secrets is the resolver the Reloader checks against, and the identity package reads through.
	Secrets *k8ssecrets.Resolver

	// Source is the source of the custom resources, which the Reloader reads, and which holds the Binding informer that the status
	// writer reads the statuses from. Nil when the config is read from files.
	Source *crsource.Source

	stopSource func() // ends the informers of the custom resources, when the Reloader reads them
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
func (c *Config) Stop() {
	if c.stopSource != nil {
		c.stopSource()
	}
	c.Secrets.Stop()
}

// Bootstrap loads the config once to learn which Secrets it refers to, starts a resolver that
// follows those and opts.ExtraSecrets and waits for their caches (an error names the cause, so a
// denied list fails at once with "forbidden", wrapping k8ssecrets.ErrAccessDenied), then starts
// the Reloader over it, which follows the Secrets of each later config. The Reloader runs until
// ctx ends. On failure nothing is left running. A config that does not load because the operator
// must fix it (config.ErrInvalid) gives an error wrapping ErrWiring, since a restart would find it again.
//
// With opts.CRDs it reads the custom resources instead (bootstrapCRDs).
func Bootstrap(ctx context.Context, client kubernetes.Interface, opts BootstrapOptions) (*Config, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.CRDs != nil {
		return bootstrapCRDs(ctx, client, opts, log)
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
		Logger: log, OnError: opts.OnReloadError, OnReport: opts.OnReport,
	})
	if err := rl.Start(ctx); err != nil {
		secrets.Stop()
		return nil, wiring(err)
	}
	return &Config{Reloader: rl, Secrets: secrets}, nil
}

// bootstrapCRDs is Bootstrap for the custom resources, which fail one object at a time. It starts the resolver (the pinned
// Secrets), then the informers of the three kinds, and reads the objects once, to find the Secret of the broker's own
// connection and follow it first: an own connection that is missing or invalid, and an own Secret that cannot be read, stop
// the start. The Secrets of the Bindings and backends are not waited for here: the Reloader follows them and does not fail
// for one that is missing or denied, which the prober reports for the Binding it belongs to. The error of each failure that
// is the operator's to fix wraps ErrWiring (a CRD that is not installed, an own connection that is not usable) or
// k8ssecrets.ErrAccessDenied or crsource.ErrAccessDenied (RBAC). On failure nothing is left running.
func bootstrapCRDs(ctx context.Context, client kubernetes.Interface, opts BootstrapOptions, log *slog.Logger) (*Config, error) {
	src, err := crsource.New(crsource.Options{Client: opts.CRDs.Client, Connection: opts.CRDs.Connection, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWiring, err)
	}
	secrets := k8ssecrets.New(client, k8ssecrets.WithLogger(log), k8ssecrets.WithPinned(opts.ExtraSecrets...))
	failed := func() { src.Stop(); secrets.Stop() }
	syncCtx, cancel := context.WithTimeout(ctx, cmp.Or(opts.SyncTimeout, defaultSyncTimeout))
	defer cancel()
	if err := secrets.Start(syncCtx); err != nil {
		failed()
		return nil, fmt.Errorf("start the secret resolver: %w", err)
	}
	if err := src.Start(syncCtx); err != nil {
		failed()
		if errors.Is(err, crsource.ErrCRDsAbsent) {
			err = fmt.Errorf("%w: %w", ErrWiring, err)
		}
		return nil, fmt.Errorf("start the custom resource source: %w", err)
	}
	set, _, err := src.Set(syncCtx)
	if err != nil {
		failed()
		return nil, fmt.Errorf("read the custom resources: %w", err)
	}
	conn, err := ownConnection(set, opts.CRDs.Connection)
	if err != nil {
		failed()
		return nil, err
	}
	own := config.SecretRead{SecretName: config.SecretName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}, Keys: config.ChatCredentialKeys()}
	if err := secrets.WatchKeys(syncCtx, []config.SecretRead{own}); err != nil {
		failed()
		return nil, fmt.Errorf("start the secret resolver: %w", err)
	}
	rl := config.NewReloader(config.ReloaderConfig{
		Source: src, Secrets: secrets, PollInterval: opts.PollInterval, SecretsInterval: opts.SecretsInterval, SecretsWait: opts.SecretsWait,
		Logger: log, OnError: opts.OnReloadError, OnReport: opts.OnReport, PerObject: true, FirstLoadWait: cmp.Or(opts.SyncTimeout, defaultSyncTimeout),
	})
	if err := rl.Start(ctx); err != nil {
		failed()
		return nil, wiring(err)
	}
	return &Config{Reloader: rl, Secrets: secrets, Source: src, stopSource: src.Stop}, nil
}

// ownConnection returns the broker's own ChatConnection of set, or an error wrapping ErrWiring that says whether it does not
// exist or does not pass Default and Validate, and why.
func ownConnection(set *config.Set, own v1alpha1.ObjectRef) (v1alpha1.ChatConnection, error) {
	name := v1alpha1.NamespacedName(own.Namespace, own.Name)
	if i := slices.IndexFunc(set.ChatConnections, func(c v1alpha1.ChatConnection) bool { return c.Namespace == own.Namespace && c.Name == own.Name }); i >= 0 {
		return set.ChatConnections[i], nil
	}
	for _, inv := range set.Invalid {
		if inv.Kind == config.KindChatConnection && inv.Namespace == own.Namespace && inv.Name == own.Name {
			return v1alpha1.ChatConnection{}, fmt.Errorf("%w: ChatConnection %s is invalid, and a broker does not start without its own connection: %s", ErrWiring, name, inv.Message)
		}
	}
	return v1alpha1.ChatConnection{}, fmt.Errorf("%w: ChatConnection %s was not found: AGENTKOURIER_CHAT_CONNECTION names the one connection this broker serves, and it must exist", ErrWiring, name)
}

// wiring wraps ErrWiring around err when the config is at fault, and returns other errors as they are.
func wiring(err error) error {
	if errors.Is(err, config.ErrInvalid) {
		return fmt.Errorf("%w: %w", ErrWiring, err)
	}
	return err
}
