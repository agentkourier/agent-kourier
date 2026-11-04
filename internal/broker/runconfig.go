package broker

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// RunConfig is what Run is built from. Everything in it is injected: Run reads no environment
// variable, and cmd/agent-kourier is the one place that does.
type RunConfig struct {
	// ConfigDir is the directory of resource files, and DBPath the SQLite file on the volume (read only with
	// the SQLite store, and required then).
	ConfigDir, DBPath string
	// Store chooses SQLite, the default, or Postgres, and where the Postgres DSN is.
	Store StoreConfig
	// Election is the leader election of a store that fences (Postgres; see store.Fenced), and is required with one.
	// SQLite has no election: its replica does the work at once.
	Election ElectionConfig
	// Listener serves /healthz, /readyz and /metrics, from the start of Run to its end.
	Listener net.Listener
	// Client is the Kubernetes client the Secret informers use. Nil means InClusterClient.
	Client kubernetes.Interface
	// SyncTimeout bounds the wait for the Secret caches; zero means Bootstrap's default.
	SyncTimeout time.Duration
	// KlogVerbosity is the verbosity requested for klog; RouteKlog holds it below 8.
	KlogVerbosity int
	Telemetry     telemetry.Config
	// AuditRetention is how long audit entries are kept; zero keeps them for ever, which is what runs until an
	// operator sets one (AGENTKOURIER_AUDIT_RETENTION). A value below MinAuditRetention is refused when the broker starts.
	AuditRetention time.Duration
	TracerProvider trace.TracerProvider // test or embedding override; caller owns its shutdown
	ServiceVersion string
	Logger         *slog.Logger // slog.Default() if nil

	// The renderer and the interactions are not here: Run builds them, from its own store, outbox and
	// Slack client, so that no config can run a broker without them.

	// newSlack builds the Slack client from its config; nil means slack.New. It is a seam for this
	// package's tests, which run Run offline with a client that does not call Slack. It is
	// unexported, so no other package, and no config, can replace the client.
	newSlack func(slack.Config) slackAPI
	// sink replaces the renderer Run builds, for this package's tests that need to see what a turn
	// or a notice was handed. Like newSlack it is unexported, so nothing else can swap the renderer.
	sink renderSink
	// reloadPoll is how often the config directory is checked, and auditStore wraps the store that
	// audit rows are written to: seams for this package's tests, so that a reload can be seen
	// without waiting 10 s and an audit write can be made to fail.
	reloadPoll time.Duration
	auditStore func(store.Audit) store.Audit
	// threadSweepEvery is how often the rows of old trigger threads are purged (an hour if zero): a seam for this
	// package's tests, which cannot wait for it.
	threadSweepEvery time.Duration
	// retentionEvery is how often the store retention loop runs (an hour if zero): a seam for this package's tests, which
	// cannot wait for it.
	retentionEvery time.Duration
	// digestEvery is how often the digest lines of stopped alerts are looked for (15 seconds if zero): a seam for this
	// package's tests, which cannot wait for it.
	digestEvery time.Duration
	// probeEvery and probeTick are how often the verification of the Bindings probes (5 minutes) and looks for a change of the config
	// (5 seconds) if zero: a seam for this package's tests, which cannot wait for them.
	probeEvery, probeTick time.Duration
	// probeGap is the least time between a pass of the verification and one that a change of the config calls for (30 seconds if zero).
	probeGap time.Duration
	// shutdownTrace is told as each shutdown step begins, so a test can see the order and act in between.
	shutdownTrace func(step string)
	// stopTrace is told the name of each stop of the interactions' step as it begins, so a test can see
	// the order inside the step, which shutdownTrace, which sees the steps, cannot.
	stopTrace func(name string)
	// leaveKlog leaves klog's process-wide logger alone: a test that runs two brokers in one process, which one
	// process never does, would otherwise have the second set it while the first's informers read it.
	leaveKlog bool
	// exit ends the process when leadership is lost: os.Exit if nil. A test records the call instead.
	exit func(code int)
	// leaseNow is the clock of the leader's lease and of the fence of its chat writes (time.Now if nil): a seam for this
	// package's tests, which cannot wait for a renew deadline or pause a process.
	leaseNow func() time.Time
	// wrapStore replaces the store Run opened with one that wraps it, for this package's tests that need a store
	// that fences (store.Fenced) over SQLite, which does not.
	wrapStore func(openedStore) openedStore
	// startTrace is told as each step of the interactions' startup is done, so a test can see their order
	// against the Slack intake opening.
	startTrace func(step string)
	// leaderLaunched is called once Run has launched the leader half (or its candidacy), before it waits for the
	// signal, so a test can hold Run there and send the signal while the leader half is up.
	leaderLaunched func()
}

// validate says whether c can run. A missing field is an error wrapping ErrWiring, which is a usage
// error, so the process exits 2 and names it, rather than failing later in some other way.
func (c RunConfig) validate() error {
	if err := c.Store.validate(); err != nil {
		return err
	}
	if c.Store.kind() == StorePostgres {
		if err := c.Election.validate(); err != nil {
			return err
		}
	}
	if c.AuditRetention < 0 || (c.AuditRetention > 0 && c.AuditRetention < MinAuditRetention) {
		return fmt.Errorf("%w: AuditRetention is %v; want 0 (keep) or at least %v", ErrWiring, c.AuditRetention, MinAuditRetention)
	}
	var missing []string
	for name, ok := range map[string]bool{
		"ConfigDir": c.ConfigDir != "", "DBPath": c.DBPath != "" || c.Store.kind() != StoreSQLite, "Listener": c.Listener != nil,
	} {
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("%w: RunConfig lacks %v", ErrWiring, missing)
	}
	return nil
}

func (c RunConfig) exitFunc() func(int) {
	if c.exit != nil {
		return c.exit
	}
	return os.Exit
}

func (c RunConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// InClusterClient returns a Kubernetes client for the API server of the cluster Agent Kourier runs in,
// from its pod's ServiceAccount, and nothing else: there is no kubeconfig path, because a kubeconfig
// on a workstation can point at any cluster, and Agent Kourier reads Secrets. Outside a cluster the error
// wraps ErrWiring and rest.ErrNotInCluster.
func InClusterClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("%w: Agent Kourier runs only in a cluster: %w", ErrWiring, err)
		}
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	// client-go's default is 5 requests a second with a burst of 10, shared by every request of this client. Each followed
	// Secret is an informer that lists once (on a server without streamed lists), and relists after a watch expires, and
	// the leader's Lease renewals go through the same limiter, so a hundred Secrets would take about 18 s to list and could
	// hold a renewal past the renew deadline. The higher limit lets a client send up to 50 requests a second, and on a
	// server without streamed lists that is up to 50 lists a second when the API server restarts and every informer lists
	// again (the burst of 100 lets the first hundred go at once). In a steady state it is one watch per Secret.
	cfg.QPS, cfg.Burst = 50, 100
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("in-cluster client: %w", err)
	}
	return client, nil
}
