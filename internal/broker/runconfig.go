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
// variable, and cmd/courier is the one place that does.
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
	KlogVerbosity  int
	Telemetry      telemetry.Config
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
	// digestEvery is how often the digest lines of stopped alerts are looked for (15 seconds if zero): a seam for this
	// package's tests, which cannot wait for it.
	digestEvery time.Duration
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
	// wrapStore replaces the store Run opened with one that wraps it, for this package's tests that need a store
	// that fences (store.Fenced) over SQLite, which does not.
	wrapStore func(openedStore) openedStore
	// startTrace is told as each step of the interactions' startup is done, so a test can see their order
	// against the Slack intake opening.
	startTrace func(step string)
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

// InClusterClient returns a Kubernetes client for the API server of the cluster courier runs in,
// from its pod's ServiceAccount, and nothing else: there is no kubeconfig path, because a kubeconfig
// on a workstation can point at any cluster, and courier reads Secrets. Outside a cluster the error
// wraps ErrWiring and rest.ErrNotInCluster.
func InClusterClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("%w: courier runs only in a cluster: %w", ErrWiring, err)
		}
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("in-cluster client: %w", err)
	}
	return client, nil
}
