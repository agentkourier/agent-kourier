package broker

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/retention"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// valid returns a RunConfig that passes validate: every required field set.
func valid(t *testing.T) RunConfig {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return RunConfig{ConfigDir: "/etc/agent-kourier", DBPath: "/data/agent-kourier.db", Listener: ln, Logger: testutil.DiscardLogger()}
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
		"the least":           {retention.MinAuditRetention, true},
		"ninety days":         {90 * 24 * time.Hour, true},
		"an hour":             {time.Hour, false},
		"a day less a moment": {retention.MinAuditRetention - time.Second, false},
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

// The admission list and the selector are for the custom resources: the files source serves the one ChatConnection of its directory, and
// each item of the list is a namespace name. The error names the item's position and not its value. With the custom resources the own
// ChatConnection is optional (spec D53), and with no list and no selector either the broker serves every namespace (spec D54); half a
// reference is refused, and so is a selector that selects nothing (labels.Nothing(), what LabelSelectorAsSelector makes of a nil one),
// which is never what an unset selector means.
func TestValidateChecksTheAdmissionList(t *testing.T) {
	crds := func(c *RunConfig, namespaces ...string) {
		c.ConfigSource, c.ChatConnection, c.ConnectionNamespaces = SourceCRDs, testConnection, namespaces
	}
	team := labels.SelectorFromSet(labels.Set{"team": "yes"})
	for name, tc := range map[string]struct {
		mutate func(*RunConfig)
		want   string // "" is accepted; otherwise a piece of the wiring error
	}{
		"none": {func(c *RunConfig) { crds(c) }, ""},
		"no own connection, a list": {func(c *RunConfig) {
			crds(c, "team-a")
			c.ChatConnection = v1alpha1.ObjectRef{}
		}, ""},
		"no own connection, no list, no selector: every namespace": {func(c *RunConfig) {
			crds(c)
			c.ChatConnection = v1alpha1.ObjectRef{}
		}, ""},
		"no own connection, a selector": {func(c *RunConfig) {
			crds(c)
			c.ChatConnection, c.ConnectionNamespaceSelector = v1alpha1.ObjectRef{}, team
		}, ""},
		"no own connection, an empty selector": {func(c *RunConfig) {
			crds(c)
			c.ChatConnection, c.ConnectionNamespaceSelector = v1alpha1.ObjectRef{}, labels.Everything()
		}, ""},
		"an own connection, a list and a selector": {func(c *RunConfig) {
			crds(c, "team-a")
			c.ConnectionNamespaceSelector = team
		}, ""},
		"a selector that selects nothing": {func(c *RunConfig) {
			crds(c)
			c.ConnectionNamespaceSelector = labels.Nothing()
		}, "ConnectionNamespaceSelector selects nothing"},
		"files refuses a selector":        {func(c *RunConfig) { c.ConnectionNamespaceSelector = team }, "ConnectionNamespaceSelector is set with the files config source"},
		"files refuses an empty selector": {func(c *RunConfig) { c.ConnectionNamespaceSelector = labels.Everything() }, "ConnectionNamespaceSelector is set with the files config source"},
		"half an own connection, a list": {func(c *RunConfig) {
			crds(c, "team-a")
			c.ChatConnection = v1alpha1.ObjectRef{Namespace: "agent-kourier-system"}
		}, "ChatConnection must be set to a namespace and a name"},
		"the other half, a list": {func(c *RunConfig) {
			crds(c, "team-a")
			c.ChatConnection = v1alpha1.ObjectRef{Name: "slack-main"}
		}, "ChatConnection must be set to a namespace and a name"},
		"some":                   {func(c *RunConfig) { crds(c, "team-a", "team-b") }, ""},
		"files refuses a list":   {func(c *RunConfig) { c.ConnectionNamespaces = []string{"team-a"} }, "ConnectionNamespaces is set with the files config source"},
		"a wildcard":             {func(c *RunConfig) { crds(c, "team-a", "*") }, "ConnectionNamespaces item 2"},
		"uppercase":              {func(c *RunConfig) { crds(c, "Team-A") }, "ConnectionNamespaces item 1"},
		"a namespace and a name": {func(c *RunConfig) { crds(c, "team-a/slack") }, "ConnectionNamespaces item 1"},
		"an empty item":          {func(c *RunConfig) { crds(c, "") }, "ConnectionNamespaces item 1"},
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
			case tc.want != "" && (strings.Contains(err.Error(), "team-a") || strings.Contains(err.Error(), "Team-A")):
				t.Errorf("validate shows a value: %v", err)
			}
		})
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
