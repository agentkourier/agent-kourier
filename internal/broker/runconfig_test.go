package broker

import (
	"errors"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

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
	return RunConfig{ConfigDir: "/etc/agent-kourier", DBPath: "/data/courier.db", Listener: ln, Logger: slog.New(slog.DiscardHandler)}
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

// The audit retention is checked before anything starts: a standby would otherwise find a value it cannot use only
// when it leads, hours later.
func TestValidateChecksTheAuditRetention(t *testing.T) {
	for name, tc := range map[string]struct {
		retention time.Duration
		ok        bool
	}{
		"keep":                {0, true},
		"the least":           {MinAuditRetention, true},
		"ninety days":         {90 * 24 * time.Hour, true},
		"an hour":             {time.Hour, false},
		"a day less a moment": {MinAuditRetention - time.Second, false},
		"negative":            {-time.Hour, false},
	} {
		c := valid(t)
		c.AuditRetention = tc.retention
		err := c.validate()
		if tc.ok && err != nil || !tc.ok && !errors.Is(err, ErrWiring) {
			t.Errorf("%s: validate = %v; want ok=%v (an ErrWiring when not)", name, err, tc.ok)
		}
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

// Agent Kourier reads Secrets and watches them through the API server it runs next to; a kubeconfig on a
// laptop is never used, because it could point at any cluster.
func TestInClusterClientRefusesOutsideACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	client, err := InClusterClient()

	if client != nil || !errors.Is(err, ErrWiring) || !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("InClusterClient = %v, %v; want no client and ErrWiring wrapping ErrNotInCluster", client, err)
	}
}

func TestValidateTheStore(t *testing.T) {
	pg := SecretKey{Namespace: "agent-kourier", Name: "agent-kourier-pg", Key: "dsn"}
	for name, tc := range map[string]struct {
		mutate func(*RunConfig)
		want   string // "" is accepted; otherwise a piece of the wiring error
	}{
		"sqlite is the default": {func(*RunConfig) {}, ""},
		"sqlite, named":         {func(c *RunConfig) { c.Store.Kind = StoreSQLite }, ""},
		"sqlite needs its path": {func(c *RunConfig) { c.DBPath = "" }, "DBPath"},
		"postgres with its Secret": {func(c *RunConfig) {
			c.DBPath = ""
			c.Store = StoreConfig{Kind: StorePostgres, PostgresDSN: pg}
			c.Election = testElection
		}, ""},
		"postgres needs the election": {func(c *RunConfig) { c.DBPath = ""; c.Store = StoreConfig{Kind: StorePostgres, PostgresDSN: pg} }, "the election lacks [Identity Name Namespace]"},
		"the election's timings must be ordered": {func(c *RunConfig) {
			c.DBPath = ""
			c.Store = StoreConfig{Kind: StorePostgres, PostgresDSN: pg}
			c.Election = testElection
			c.Election.LeaseDuration, c.Election.RenewDeadline = 5*time.Second, 5*time.Second
		}, "lease duration"},
		"postgres needs the Secret": {func(c *RunConfig) { c.Store.Kind = StorePostgres }, "Secret that holds its DSN"},
		"postgres needs the whole key": {func(c *RunConfig) {
			c.Store = StoreConfig{Kind: StorePostgres, PostgresDSN: SecretKey{Namespace: "agent-kourier", Name: "x"}}
		}, "Secret that holds its DSN"},
		"sqlite refuses a DSN Secret":  {func(c *RunConfig) { c.Store.PostgresDSN = pg }, "a Postgres setting"},
		"sqlite refuses role timeouts": {func(c *RunConfig) { c.Store.PostgresRoleTimeouts = true }, "a Postgres setting"},
		"an unknown store is refused":  {func(c *RunConfig) { c.Store.Kind = "mysql" }, `unknown store "mysql"`},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			tc.mutate(&c)
			err := c.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("validate = %v, want nil", err)
			case tc.want != "" && (!errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("validate = %v, want an ErrWiring saying %q", err, tc.want)
			}
		})
	}
}
