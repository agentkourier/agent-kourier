package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// RunConfig is what Run is built from. Everything in it is injected: Run reads no environment
// variable, and cmd/courier is the one place that does.
type RunConfig struct {
	// ConfigDir is the directory of resource files, and DBPath the SQLite file on the volume.
	ConfigDir, DBPath string
	// Listener serves /healthz, /readyz and /metrics, from the start of Run to its end.
	Listener net.Listener
	// Client is the Kubernetes client the Secret informers use. Nil means InClusterClient.
	Client kubernetes.Interface
	// SyncTimeout bounds the wait for the Secret caches; zero means Bootstrap's default.
	SyncTimeout time.Duration
	// KlogVerbosity is the verbosity requested for klog; RouteKlog holds it below 8.
	KlogVerbosity int
	Logger        *slog.Logger // slog.Default() if nil

	// Sink renders agent output, and Interactions takes the presses of buttons. Both are required:
	// until the renderer and the interactions exist (WP-35h) only a testhooks build may go without,
	// by NoRenderer and NoInteractions, which the default build does not have.
	Sink         session.Sink
	Interactions func(ctx context.Context, i slack.Interaction) error

	// newSlack builds the Slack client from its config; nil means slack.New. It is a seam for this
	// package's tests, which run Run offline with a client that does not call Slack. It is
	// unexported, so no other package, and no config, can replace the client.
	newSlack func(slack.Config) slackAPI

	testOptions // NoRenderer and NoInteractions in a testhooks build; nothing otherwise
}

// validate says whether c can run. A missing field is an error wrapping ErrWiring, which is a usage
// error, so the process exits 2 and names it, rather than failing later in some other way.
func (c RunConfig) validate() error {
	var missing []string
	for name, ok := range map[string]bool{
		"ConfigDir": c.ConfigDir != "", "DBPath": c.DBPath != "", "Listener": c.Listener != nil,
		"Sink":         c.Sink != nil || c.allowsNoRenderer(),
		"Interactions": c.Interactions != nil || c.allowsNoInteractions(),
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

func (c RunConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// logTestOptions logs, once and loudly, what a testhooks build is running without. Run calls it as
// its first act, not validate, which may be called any number of times. In the default build there is
// nothing to say.
func (c RunConfig) logTestOptions() { c.warnTestOptions(c.logger()) }

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
