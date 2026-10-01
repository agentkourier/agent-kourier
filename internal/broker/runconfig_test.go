package broker

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// noSink is a session.Sink that does nothing, so a config can have one without a renderer.
type noSink struct{}

func (noSink) Turn(context.Context, session.TurnRef) agent.Handler { return func(agent.Event) {} }
func (noSink) Notice(context.Context, session.Key, session.Notice) {}

// valid returns a RunConfig that passes validate: every required field set.
func valid(t *testing.T) RunConfig {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return RunConfig{
		ConfigDir: "/etc/courier", DBPath: "/data/courier.db", Listener: ln,
		Sink: noSink{}, Interactions: func(context.Context, slack.Interaction) error { return nil },
		Logger: slog.New(slog.DiscardHandler),
	}
}

func TestValidateAcceptsAWholeConfig(t *testing.T) {
	if err := valid(t).validate(); err != nil {
		t.Errorf("validate = %v, want nil", err)
	}
}

func TestValidateRefusesWhatIsMissingAsAWiringError(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*RunConfig)
		names  string
	}{
		"no renderer":     {func(c *RunConfig) { c.Sink = nil }, "Sink"},
		"no interactions": {func(c *RunConfig) { c.Interactions = nil }, "Interactions"},
		"no config dir":   {func(c *RunConfig) { c.ConfigDir = "" }, "ConfigDir"},
		"no database":     {func(c *RunConfig) { c.DBPath = "" }, "DBPath"},
		"no listener":     {func(c *RunConfig) { c.Listener = nil }, "Listener"},
	} {
		c := valid(t)
		tc.mutate(&c)
		err := c.validate()
		if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s: validate = %v, want an ErrWiring naming %s", name, err, tc.names)
		}
	}
}

// Courier reads Secrets and watches them through the API server it runs next to; a kubeconfig on a
// laptop is never used, because it could point at any cluster.
func TestInClusterClientRefusesOutsideACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	client, err := InClusterClient()

	if client != nil || !errors.Is(err, ErrWiring) || !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("InClusterClient = %v, %v; want no client and ErrWiring wrapping ErrNotInCluster", client, err)
	}
}
