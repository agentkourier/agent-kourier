package broker

import (
	"errors"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// valid returns a RunConfig that passes validate: every required field set.
func valid(t *testing.T) RunConfig {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return RunConfig{ConfigDir: "/etc/courier", DBPath: "/data/courier.db", Listener: ln, Logger: slog.New(slog.DiscardHandler)}
}

// No build can start without the renderer or the interactions, or with different ones, and none can
// point Slack's API somewhere else, which would send the bot token there: the fields are not in RunConfig,
// so no code, and no config, can set them. (The Slack URL comes from testhooks.SlackAPIURL, which is ""
// in a production build.)
func TestRunConfigHasNoWayToLeaveOutOrSwapTheRendererTheInteractionsOrTheSlackURL(t *testing.T) {
	typ := reflect.TypeOf(RunConfig{})
	for _, name := range []string{"Sink", "NoRenderer", "Interactions", "NoInteractions", "SlackAPIURL", "APIURL"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Errorf("RunConfig has a %s field", name)
		}
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
		"no config dir": {func(c *RunConfig) { c.ConfigDir = "" }, "ConfigDir"},
		"no database":   {func(c *RunConfig) { c.DBPath = "" }, "DBPath"},
		"no listener":   {func(c *RunConfig) { c.Listener = nil }, "Listener"},
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
