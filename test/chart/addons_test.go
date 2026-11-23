package chart_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"
)

// The add-ons (addons.<name>) render from one set of templates (templates/addons/_addon.tpl) and each one's own
// (templates/addons/<name>/env.tpl). An add-on's objects are <fullname>-<name>, and its pods are labelled
// app.kubernetes.io/name=agent-kourier-<name>, which none of Agent Kourier's selectors match. Every test in this file
// runs for each add-on of addOns; axaddon_test.go tests the AX add-on's own keys.

// addOn is what these tests need to know of an add-on.
type addOn struct {
	name string // its key under addons, and the suffix of its objects' names

	// set is the --set of the add-on with the least it needs (SQLite, one replica), which on() puts on the minimal install
	// and TestAddOnsSideBySide adds to another set of arguments.
	set      string
	postgres string     // the --set that switches on to Postgres
	sqlite   string     // its ci values file on SQLite with one replica (values() name)
	pg       string     // its ci values file on Postgres with two replicas, and every optional setting
	more     [][]string // more sets of arguments of its own, which must render

	// own are the add-on's own keys under addons.<name>, as dotted paths; every other key is one every add-on has.
	own []string

	contract []string // every variable the add-on reads that the chart may set: the chart renders no other, apart from extraEnv
	never    []string // settings of the add-on that the chart never sets, beside AUTH_DISABLED

	onEnv       map[string]string    // the environment that on renders as release team in namespace ops (from a source: "")
	pgEnv       map[string]string    // values the pg file renders, among others
	sqliteFrom  map[string]secretRef // the variables that the sqlite file takes from a Secret, and nothing else from a source
	ownOff      [][]string           // its own settings, which a block that is off leaves unread
	ownRefusals []refusal            // the refusals of its own keys

	// policies are the add-on's NetworkPolicy for values files of its own: the ingress rules (Agent Kourier, then the
	// scrapers where metricsFrom names them, then the add-on's own replicas), whether the last is there, and the ports of
	// the egress rules in order.
	policies []policyCase
}

// secretRef names a Secret and its key.
type secretRef struct{ name, key string }

// refusal is a render that must fail with an error that contains want: args after the add-on's on(), or after base where it is set.
type refusal struct {
	name string
	args []string
	want string
	base []string
}

// policyCase is one of addOn.policies.
type policyCase struct {
	values      string
	ingress     int
	selfIngress bool
	egressPorts []int
}

// on is the add-on with the least it needs, on the minimal install.
func (a addOn) on() []string { return []string{"-f", values("minimal"), "--set", a.set} }

// with is on plus more arguments.
func (a addOn) with(more ...string) []string { return append(a.on(), more...) }

// objectName is the name of the add-on's objects in a release agent-kourier: agent-kourier-<name>, also its pods' app.kubernetes.io/name.
func (a addOn) objectName() string { return "agent-kourier-" + a.name }

// valueSets are every set of arguments of the add-on that the tests render, each of which must work.
func (a addOn) valueSets() [][]string {
	return append([][]string{a.on(), {"-f", values(a.sqlite)}, {"-f", values(a.pg)}}, a.more...)
}

var addOns = []addOn{
	{
		name: "cma",
		set: "addons.cma.enabled=true,addons.cma.agentId=agent_x,addons.cma.environmentId=env_x," +
			"addons.cma.anthropicApiKeySecret.name=anthropic,addons.cma.anthropicApiKeySecret.key=apiKey,addons.cma.auth.tokenSecret.name=cma-token",
		postgres: "addons.cma.store.type=postgres,addons.cma.store.postgres.dsnSecret.name=cma-pg,addons.cma.store.postgres.dsnSecret.key=dsn",
		sqlite:   "cma",
		pg:       "cma-postgres",
		own:      []string{"agentId", "environmentId", "anthropicApiKeySecret", "toolResultTimeout", "networkPolicy.anthropic"},
		contract: []string{
			"CMA_AGENT_ID", "CMA_ENVIRONMENT_ID", "ANTHROPIC_API_KEY", "LISTEN_ADDR", "PUBLIC_URL", "SESSION_IDLE_TTL", "TOOL_RESULT_TIMEOUT",
			"AUTH_TOKEN_FILE", "AUTH_HEADER", "STORE", "SQLITE_PATH", "POSTGRES_DSN", "TASK_RETENTION", "LEASE_DURATION", "LEASE_RENEW_DEADLINE",
			"LEASE_RETRY_PERIOD", "ADVERTISE_URL", "POD_NAME", "LOG_LEVEL",
			"POD_IP",     // the downward API's pod address, which ADVERTISE_URL refers to
			"GOMEMLIMIT", // the Go runtime's, from the memory limit
		},
		onEnv: map[string]string{
			"CMA_AGENT_ID": "agent_x", "CMA_ENVIRONMENT_ID": "env_x", "ANTHROPIC_API_KEY": "", "LISTEN_ADDR": ":8090",
			"PUBLIC_URL": "http://team-agent-kourier-cma.ops.svc:8090/a2a", "AUTH_TOKEN_FILE": "/etc/agent-kourier-cma/auth/tokens",
			"STORE": "sqlite", "SQLITE_PATH": "/data/cma.db", "LOG_LEVEL": "info", "GOMEMLIMIT": "214748364",
		},
		pgEnv: map[string]string{
			"STORE": "postgres", "AUTH_HEADER": "X-Cma-Token", "SESSION_IDLE_TTL": "720h", "TOOL_RESULT_TIMEOUT": "0", "TASK_RETENTION": "168h",
			"LEASE_DURATION": "20s", "LEASE_RENEW_DEADLINE": "12s", "LEASE_RETRY_PERIOD": "3s", "LOG_LEVEL": "debug", "ADVERTISE_URL": "http://$(POD_IP):8090",
			"OTEL_TRACES_EXPORTER": "otlp",
		},
		sqliteFrom: map[string]secretRef{"ANTHROPIC_API_KEY": {"anthropic", "apiKey"}},
		ownOff:     [][]string{{"--set", "addons.cma.agentId=agent_x"}},
		ownRefusals: []refusal{
			{"no agent", []string{"--set", "addons.cma.agentId="}, "addons.cma.enabled needs addons.cma.agentId", nil},
			{"no environment", []string{"--set", "addons.cma.environmentId="}, "addons.cma.enabled needs addons.cma.environmentId", nil},
			{"no API key Secret", []string{"--set", "addons.cma.anthropicApiKeySecret.name="}, "addons.cma.anthropicApiKeySecret.name and addons.cma.anthropicApiKeySecret.key", nil},
			{"an API key Secret with no key", []string{"--set", "addons.cma.anthropicApiKeySecret.key="}, "addons.cma.anthropicApiKeySecret.name and addons.cma.anthropicApiKeySecret.key", nil},
			{"a timeout that is not a duration", []string{"--set", "addons.cma.toolResultTimeout=later"}, "/addons/cma/toolResultTimeout", nil},
			{"a ServiceAccount setting, which the add-on has not", []string{"--set", "addons.cma.serviceAccount.annotations.a=b"}, "additional properties 'serviceAccount' not allowed", nil},
		},
		policies: []policyCase{
			// Agent Kourier and the scrapers; DNS (two ports), the Anthropic API.
			{"cma", 2, false, []int{53, 53, 443}},
			// Agent Kourier and the other replicas; DNS, the Anthropic API, the other replicas, the database, the collector.
			{"cma-postgres", 2, true, []int{53, 53, 443, 8090, 5432, 4318}},
		},
	},
	{
		name: "ax",
		// SQLite, one replica, AWS credentials from the SDK's default chain.
		set: "addons.ax.enabled=true,addons.ax.model.region=us-east-1,addons.ax.model.name=us.anthropic.claude-haiku-4-5-20251001-v1:0," +
			"addons.ax.auth.tokenSecret.name=ax-token",
		postgres: "addons.ax.store.type=postgres,addons.ax.store.postgres.dsnSecret.name=ax-pg,addons.ax.store.postgres.dsnSecret.key=dsn",
		// Its values files are ax-addon-*: ax-values.yaml is the AX kind tier's Agent Kourier, not this add-on.
		sqlite: "ax-addon",
		pg:     "ax-addon-postgres",
		more:   [][]string{axAnthropicOn, {"-f", values("ax-addon-anthropic")}},
		own:    []string{"model", "bedrock", "anthropicApiKeySecret", "serviceAccount", "networkPolicy.model"},
		contract: []string{
			"MODEL_PROVIDER", "MODEL_REGION", "MODEL_NAME", "AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_API_KEY", "LISTEN_ADDR", "PUBLIC_URL", "AUTH_TOKEN_FILE",
			"AUTH_HEADER", "STORE", "SQLITE_PATH", "POSTGRES_DSN", "TASK_RETENTION", "SESSION_IDLE_TTL", "LEASE_DURATION", "LEASE_RENEW_DEADLINE",
			"LEASE_RETRY_PERIOD", "ADVERTISE_URL", "POD_NAME", "LOG_LEVEL",
			"POD_IP",     // the downward API's pod address, which ADVERTISE_URL refers to
			"GOMEMLIMIT", // the Go runtime's, from the memory limit
		},
		// RUNNER_URL and OIDC are the AX route's, STATE_DIR is gone (the add-on refuses it), and OTEL_SERVICE_NAME is left
		// to the add-on's default.
		never: []string{"RUNNER_URL", "OIDC_ISSUER", "OIDC_AUDIENCE", "OIDC_SUBJECT", "STATE_DIR", "OTEL_SERVICE_NAME"},
		onEnv: map[string]string{
			"MODEL_PROVIDER": "bedrock", "MODEL_REGION": "us-east-1", "MODEL_NAME": "us.anthropic.claude-haiku-4-5-20251001-v1:0", "LISTEN_ADDR": ":8090",
			"PUBLIC_URL": "http://team-agent-kourier-ax.ops.svc:8090/a2a", "AUTH_TOKEN_FILE": "/etc/agent-kourier-ax/auth/tokens",
			"STORE": "sqlite", "SQLITE_PATH": "/data/ax.db", "LOG_LEVEL": "info", "GOMEMLIMIT": "429496729",
		},
		pgEnv: map[string]string{
			"STORE": "postgres", "AUTH_HEADER": "X-Ax-Token", "SESSION_IDLE_TTL": "720h", "TASK_RETENTION": "0",
			"LEASE_DURATION": "20s", "LEASE_RENEW_DEADLINE": "12s", "LEASE_RETRY_PERIOD": "3s", "LOG_LEVEL": "debug", "ADVERTISE_URL": "http://$(POD_IP):8090",
			"MODEL_REGION": "eu-west-1", "MODEL_NAME": "eu.anthropic.claude-haiku-4-5-20251001-v1:0", "OTEL_TRACES_EXPORTER": "otlp",
		},
		// No Bedrock key in ax-addon-values.yaml: the AWS SDK's default chain finds the credentials.
		sqliteFrom: map[string]secretRef{},
		ownOff: [][]string{
			{"--set", "addons.ax.bedrock.apiKeySecret.name=bedrock"},
			{"--set", "addons.ax.model.provider=anthropic,addons.ax.model.region=us-east-1,addons.ax.anthropicApiKeySecret.key=apiKey"},
		},
		ownRefusals: axRefusals,
		policies: []policyCase{
			// Agent Kourier and the scrapers; DNS (two ports), Bedrock, and from extraEgress the EKS Pod Identity Agent.
			{"ax-addon", 2, false, []int{53, 53, 443, 80}},
			// Agent Kourier and the other replicas; DNS, Bedrock, the other replicas, the database, the collector.
			{"ax-addon-postgres", 2, true, []int{53, 53, 443, 8090, 5432, 4318}},
			// Agent Kourier alone (no metricsFrom); DNS and the Anthropic API.
			{"ax-addon-anthropic", 1, false, []int{53, 53, 443}},
		},
	},
}

// eachAddOn runs test as a parallel subtest per add-on.
func eachAddOn(t *testing.T, test func(t *testing.T, a addOn)) {
	t.Helper()
	helmPath(t)
	for _, a := range addOns {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			test(t, a)
		})
	}
}

// deployment finds the add-on's Deployment, in a release agent-kourier, and its environment by name.
func (a addOn) deployment(t *testing.T, objs []object) (appsv1.Deployment, map[string]corev1.EnvVar) {
	t.Helper()
	var d appsv1.Deployment
	find(t, objs, "Deployment", a.objectName(), &d)
	env := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return d, env
}

// labelValues are the values of an object's labels.
func (o object) labelValues() []string {
	var out []string
	for _, v := range sub(sub(o, "metadata"), "labels") {
		out = append(out, str(v))
	}
	return out
}

// TestAddOnsAreOneList: the chart's list of add-ons (agentkourier.addon.names), the directories of templates/addons, the blocks of
// addons in values.yaml and in the values schema, and the add-ons these tests cover are the same names.
func TestAddOnsAreOneList(t *testing.T) {
	t.Parallel()
	shared, err := os.ReadFile(filepath.Join(chartDir, "templates", "addons", "_addon.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`define "agentkourier\.addon\.names" -}}([^{]*){{`).FindSubmatch(shared)
	if m == nil {
		t.Fatal("templates/addons/_addon.tpl has no one-line agentkourier.addon.names, which this test reads")
	}
	list := strings.Fields(string(m[1]))

	entries, err := os.ReadDir(filepath.Join(chartDir, "templates", "addons"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	blocks, schemas := addOnBlocks(t)
	var tested []string
	for _, a := range addOns {
		tested = append(tested, a.name)
	}
	want := slices.Sorted(slices.Values(list))
	for what, got := range map[string][]string{
		"templates/addons directories": dirs, "values.yaml addons": slices.Collect(maps.Keys(blocks)),
		"values.schema.json addons": slices.Collect(maps.Keys(schemas)), "addOns of these tests": tested,
	} {
		if slices.Sort(got); !slices.Equal(got, want) {
			t.Errorf("%s %v; want agentkourier.addon.names %v", what, got, want)
		}
	}
}

// addOnBlocks are addons.<name> of values.yaml, and the schema of each, by name.
func addOnBlocks(t *testing.T) (blocks, schemas map[string]map[string]any) {
	t.Helper()
	var v struct {
		Addons map[string]map[string]any `json:"addons"`
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Addons struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"addons"`
		} `json:"properties"`
	}
	raw, err = os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return v.Addons, s.Properties.Addons.Properties
}

// TestAddOnValuesShareTheCommonKeys: apart from each add-on's own keys, every add-on's values block has the same keys, and the values
// schema holds them to the same rules (descriptions aside), so the add-ons' common templates read the same settings from each.
func TestAddOnValuesShareTheCommonKeys(t *testing.T) {
	t.Parallel()
	blocks, schemas := addOnBlocks(t)
	var first addOn
	var firstKeys []string
	var firstSchema map[string]any
	for i, a := range addOns {
		block, rules := blocks[a.name], schemas[a.name]
		if block == nil || rules == nil {
			t.Fatalf("addons.%s: values %v, schema %v; want both", a.name, block != nil, rules != nil)
		}
		for _, own := range a.own {
			if !removePath(block, own) {
				t.Errorf("addons.%s.%s, one of the add-on's own keys, is not in values.yaml", a.name, own)
			}
			if !removePath(sub(rules, "properties"), strings.ReplaceAll(own, ".", ".properties.")) {
				t.Errorf("addons.%s.%s, one of the add-on's own keys, is not in values.schema.json", a.name, own)
			}
		}
		keys := keyPaths(block, "")
		rules = withoutDescriptions(rules).(map[string]any)
		if i == 0 {
			first, firstKeys, firstSchema = a, keys, rules
			continue
		}
		if !slices.Equal(keys, firstKeys) {
			t.Errorf("the common keys of addons.%s %v differ from addons.%s's %v", a.name, keys, first.name, firstKeys)
		}
		if !equality.Semantic.DeepEqual(rules, firstSchema) {
			t.Errorf("the schema of addons.%s's common keys differs from addons.%s's:\n%s", a.name, first.name, diff(t, firstSchema, rules))
		}
	}
}

// removePath deletes a dotted path from a map and reports whether it was there.
func removePath(m map[string]any, path string) bool {
	head, rest, nested := strings.Cut(path, ".")
	if !nested {
		_, has := m[head]
		delete(m, head)
		return has
	}
	next, ok := m[head].(map[string]any)
	return ok && removePath(next, rest)
}

// keyPaths are the dotted paths of every key of a values block, sorted.
func keyPaths(m map[string]any, prefix string) []string {
	var out []string
	for k, v := range m {
		out = append(out, prefix+k)
		if next, ok := v.(map[string]any); ok {
			out = append(out, keyPaths(next, prefix+k+".")...)
		}
	}
	slices.Sort(out)
	return out
}

// withoutDescriptions is a schema with every description removed.
func withoutDescriptions(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			if k != "description" {
				out[k] = withoutDescriptions(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = withoutDescriptions(x)
		}
		return out
	default:
		return v
	}
}

// TestAddOnIsOffAndInertByDefault: the block is off by default, and while it is off nothing of it is rendered or checked: the minimal install
// renders the same with the block's settings changed, with the block absent (a release upgraded with --reuse-values from a chart before
// it), and with the NetworkPolicy on, which then has no rule for the add-on.
func TestAddOnIsOffAndInertByDefault(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		p := "addons." + a.name
		for _, base := range [][]string{
			{"-f", values("minimal")},
			{"-f", values("minimal"), "--set", "networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"},
		} {
			want, err := render(t.Context(), t, "agent-kourier", "agent-kourier", base...)
			if err != nil {
				t.Fatal(err)
			}
			for _, more := range append([][]string{
				{"--set", p + ".enabled=false"},
				{"--set", "addons=null"},
				{"--set", p + "=null"},
				// Settings the checks would refuse if the block were on: off, they are not looked at.
				{"--set", p + ".replicaCount=3," + p + ".leaderElection.leaseDuration=20s," + p + ".store.postgres.dsnSecret.name=pg"},
			}, a.ownOff...) {
				got, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), more...)...)
				if err != nil {
					t.Errorf("%v: %v", more, err)
					continue
				}
				if got != want {
					t.Errorf("%v changes the render of an install without the add-on:\n%s", more, firstDifference(want, got))
				}
			}
			for _, o := range decode(t, want) {
				if strings.HasSuffix(o.name(), "-"+a.name) || slices.Contains(o.labelValues(), a.objectName()) {
					t.Errorf("an install without the add-on rendered %s %s", o.kind(), o.name())
				}
			}
		}
	})
}

// TestAddOnRefusals: what an enabled add-on's values must not get past, the common keys' and its own.
func TestAddOnRefusals(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		p := "addons." + a.name
		s := "/addons/" + a.name
		tests := []refusal{
			{"no token Secret", []string{"--set", p + ".auth.tokenSecret.name="}, p + ".auth.tokenSecret.name and " + p + ".auth.tokenSecret.key", nil},
			{"a token Secret with no key", []string{"--set", p + ".auth.tokenSecret.key="}, p + ".auth.tokenSecret.name and " + p + ".auth.tokenSecret.key", nil},
			{"the caller check turned off", []string{"--set", p + ".auth.disabled=true"}, "additional properties 'disabled' not allowed", nil},
			{"two replicas on SQLite", []string{"--set", p + ".replicaCount=2"}, p + ".replicaCount > 1 needs " + p + ".store.type postgres", nil},
			{"Postgres without a DSN Secret", []string{"--set", p + ".store.type=postgres"}, p + ".store.postgres.dsnSecret.name and " + p + ".store.postgres.dsnSecret.key", nil},
			{"Postgres with a Secret and no key", []string{"--set", p + ".store.type=postgres," + p + ".store.postgres.dsnSecret.name=pg"}, p + ".store.postgres.dsnSecret.name and " + p + ".store.postgres.dsnSecret.key", nil},
			{"a DSN Secret with SQLite", []string{"--set", p + ".store.postgres.dsnSecret.name=pg," + p + ".store.postgres.dsnSecret.key=dsn"}, p + ".store.postgres.dsnSecret is for " + p + ".store.type postgres", nil},
			{"an election setting with SQLite", []string{"--set", p + ".leaderElection.retryPeriod=3s"}, p + ".leaderElection is for " + p + ".store.type postgres", nil},
			{"a claim with Postgres", []string{"--set", a.postgres + "," + p + ".persistence.existingClaim=mine"}, p + ".persistence.existingClaim is for " + p + ".store.type sqlite", nil},
			{"a NetworkPolicy with Postgres and no database", []string{"--set", a.postgres + ",networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"}, p + ".networkPolicy.postgres.to", nil},
			{"an unknown store", []string{"--set", p + ".store.type=mysql"}, s + "/store/type", nil},
			{"a duration that is not one", []string{"--set", p + ".sessionIdleTTL=forever"}, s + "/sessionIdleTTL", nil},
			{"a retention that is not a duration", []string{"--set", p + ".taskRetention=a-month"}, s + "/taskRetention", nil},
			{"an election timing that is not a duration", []string{"--set", a.postgres + "," + p + ".leaderElection.leaseDuration=soon"}, s + "/leaderElection/leaseDuration", nil},
			{"a header name with a space", []string{"--set", p + ".auth.header=X Token"}, s + "/auth/header", nil},
			{"a reserved header", []string{"--set", p + ".auth.header=Cookie"}, s + "/auth/header", nil},
			{"a reserved header in another case", []string{"--set", p + ".auth.header=x-user-id"}, s + "/auth/header", nil},
			{"the longest reserved header", []string{"--set", p + ".auth.header=X-KAGENT-INSECURE-RUNTIME-IDENTITY"}, s + "/auth/header", nil},
			{"a malformed digest", []string{"--set", p + ".image.digest=latest"}, s + "/image/digest", nil},
			{"an unknown log level", []string{"--set", p + ".logLevel=trace"}, s + "/logLevel", nil},
			{"a DSN with a password in the add-on's values", []string{"--set-json", p + `.extraEnv=[{"name":"X","value":"postgres://u:hunter2@db:5432/u"}]`}, "looks like a Postgres DSN with a password", nil},
		}
		for _, tc := range append(tests, a.ownRefusals...) {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				args := a.with(tc.args...)
				if tc.base != nil {
					args = append(slices.Clone(tc.base), tc.args...)
				}
				_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", args...)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("render error = %v; want one containing %q", err, tc.want)
				}
			})
		}
	})
}

// TestAddOnDeploymentIsSafe: the add-on's pod has the broker's hardening, no Kubernetes API token, its credentials from Secrets (or the
// ServiceAccount) only, only /data and /tmp writable, the accepted tokens as a file the kubelet keeps current, and the probes on its one port.
func TestAddOnDeploymentIsSafe(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.sqlite))
		d, env := a.deployment(t, objs)
		pod := d.Spec.Template.Spec
		if len(pod.Containers) != 1 {
			t.Fatalf("containers %+v; want one", pod.Containers)
		}
		c := pod.Containers[0]

		if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Errorf("replicas %d, strategy %s; want one pod and Recreate on SQLite", *d.Spec.Replicas, d.Spec.Strategy.Type)
		}
		sc, psc := c.SecurityContext, pod.SecurityContext
		if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
			psc == nil || !*psc.RunAsNonRoot || *psc.RunAsUser != 65532 || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("security contexts %+v and %+v; want non-root, RuntimeDefault seccomp, a read-only root, no escalation, every capability dropped", psc, sc)
		}
		var writable []string
		for _, m := range c.VolumeMounts {
			if !m.ReadOnly {
				writable = append(writable, m.MountPath)
			}
		}
		if slices.Sort(writable); !slices.Equal(writable, []string{"/data", "/tmp"}) {
			t.Errorf("writable mounts %v; want /data and /tmp alone", writable)
		}
		var sa corev1.ServiceAccount
		find(t, objs, "ServiceAccount", a.objectName(), &sa)
		if pod.ServiceAccountName != a.objectName() || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
			sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
			t.Errorf("ServiceAccount %q, pod automount %v, ServiceAccount automount %v; want the add-on's own, with no token mounted", pod.ServiceAccountName, pod.AutomountServiceAccountToken, sa.AutomountServiceAccountToken)
		}
		if slices.ContainsFunc(objs, func(o object) bool {
			return (o.kind() == "Role" || o.kind() == "RoleBinding") && strings.HasSuffix(o.name(), "-"+a.name)
		}) {
			t.Errorf("objects %v; the add-on is granted nothing", kinds(objs))
		}
		// The startup probe asks /readyz, so it bounds the leader's start-up work; liveness asks /healthz, which a long takeover
		// does not fail.
		if c.StartupProbe.HTTPGet.Path != "/readyz" || c.LivenessProbe.HTTPGet.Path != "/healthz" || c.ReadinessProbe.HTTPGet.Path != "/readyz" ||
			c.ReadinessProbe.HTTPGet.Port.String() != "http" {
			t.Errorf("probes %s, %s, %s on %s; want /readyz, /healthz and /readyz on http", c.StartupProbe.HTTPGet.Path, c.LivenessProbe.HTTPGet.Path, c.ReadinessProbe.HTTPGet.Path, c.ReadinessProbe.HTTPGet.Port.String())
		}
		if len(c.Ports) != 1 || c.Ports[0].Name != "http" || c.Ports[0].ContainerPort != 8090 || c.Ports[0].HostPort != 0 || pod.HostNetwork {
			t.Errorf("ports %+v, hostNetwork %v; want http on 8090, never on the host", c.Ports, pod.HostNetwork)
		}
		if c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Memory().IsZero() {
			t.Errorf("resources %+v; want memory requests and limits", c.Resources)
		}
		from := map[string]secretRef{}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.SecretKeyRef; r == nil || e.Value != "" {
				t.Errorf("env %s from %+v; on SQLite a variable from a source is from a Secret", e.Name, e.ValueFrom)
			} else {
				from[e.Name] = secretRef{r.Name, r.Key}
			}
		}
		if !maps.Equal(from, a.sqliteFrom) {
			t.Errorf("env from Secrets %v; want %v", from, a.sqliteFrom)
		}

		// The tokens: a read-only Secret volume, the one key as the file AUTH_TOKEN_FILE names, never by subPath (which the kubelet does not update).
		var auth *corev1.Volume
		for i := range pod.Volumes {
			if pod.Volumes[i].Name == "auth" {
				auth = &pod.Volumes[i]
			}
		}
		token := a.name + "-token"
		if auth == nil || auth.Secret == nil || auth.Secret.SecretName != token || len(auth.Secret.Items) != 1 || auth.Secret.Items[0].Key != "token" ||
			auth.Secret.DefaultMode == nil || *auth.Secret.DefaultMode != 0o440 {
			t.Fatalf("volume auth %+v; want the Secret %s's key token, readable by the pod's group alone", auth, token)
		}
		mount := c.VolumeMounts[slices.IndexFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "auth" })]
		if !mount.ReadOnly || mount.SubPath != "" || env["AUTH_TOKEN_FILE"].Value != mount.MountPath+"/"+auth.Secret.Items[0].Path {
			t.Errorf("mount %+v, AUTH_TOKEN_FILE %q; want the file of the read-only mount, without subPath", mount, env["AUTH_TOKEN_FILE"].Value)
		}
	})
}

// TestAddOnImage: the add-on's image defaults to its repository at the chart's appVersion, as Agent Kourier's does, and a digest wins.
func TestAddOnImage(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil || chart.AppVersion == "" {
		t.Fatalf("Chart.yaml appVersion %q: %v", chart.AppVersion, err)
	}
	eachAddOn(t, func(t *testing.T, a addOn) {
		repository := "ghcr.io/agentkourier/" + a.objectName()
		d, _ := a.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", a.on()...))
		if got, want := d.Spec.Template.Spec.Containers[0].Image, repository+":"+chart.AppVersion; got != want {
			t.Errorf("image %q; want %q", got, want)
		}
		digest := "sha256:" + strings.Repeat("cd", 32)
		d, _ = a.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", a.with("--set", "addons."+a.name+".image.tag=v9.9.9,addons."+a.name+".image.digest="+digest)...))
		if got, want := d.Spec.Template.Spec.Containers[0].Image, repository+"@"+digest; got != want {
			t.Errorf("image %q; want %q: a digest wins over the tag", got, want)
		}
	})
}

// TestAddOnEnvironmentIsTheSettingsContract: the chart sets the add-on's variables, and no other, from the values; an empty optional value
// renders no variable, so that the add-on's own default holds.
func TestAddOnEnvironmentIsTheSettingsContract(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		// A release and namespace of another name: the URL follows the add-on's Service.
		objs := mustRender(t, "team", "ops", a.on()...)
		var d appsv1.Deployment
		find(t, objs, "Deployment", "team-"+a.objectName(), &d)
		got := map[string]string{}
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			got[e.Name] = e.Value
		}
		if !maps.Equal(got, a.onEnv) {
			t.Errorf("env %v; want %v", got, a.onEnv)
		}
		var svc corev1.Service
		find(t, objs, "Service", "team-"+a.objectName(), &svc)

		// Postgres, and every optional setting.
		d, env := a.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.pg)))
		for name, value := range a.pgEnv {
			if env[name].Value != value {
				t.Errorf("%s %q; want %q", name, env[name].Value, value)
			}
		}
		if r := env["POSTGRES_DSN"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != a.name+"-pg" || r.SecretKeyRef.Key != "dsn" {
			t.Errorf("POSTGRES_DSN from %+v; want the Secret %s-pg's key dsn", r, a.name)
		}
		for name, path := range map[string]string{"POD_NAME": "metadata.name", "POD_IP": "status.podIP"} {
			if r := env[name].ValueFrom; r == nil || r.FieldRef == nil || r.FieldRef.FieldPath != path {
				t.Errorf("%s from %+v; want the downward API's %s", name, r, path)
			}
		}
		// Kubernetes expands $(POD_IP) only from a variable defined before the one that refers to it.
		names := make([]string, 0, len(d.Spec.Template.Spec.Containers[0].Env))
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			names = append(names, e.Name)
		}
		if slices.Index(names, "POD_IP") > slices.Index(names, "ADVERTISE_URL") {
			t.Errorf("env order %v; POD_IP must come before ADVERTISE_URL", names)
		}
		if _, has := env["SQLITE_PATH"]; has {
			t.Errorf("SQLITE_PATH with Postgres")
		}

		for _, args := range a.valueSets() {
			_, env := a.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", args...))
			for name := range env {
				if !slices.Contains(a.contract, name) && !strings.HasPrefix(name, "OTEL_") {
					t.Errorf("%v: env %s is not a setting of the add-on", args, name)
				}
			}
			for _, name := range append([]string{"AUTH_DISABLED"}, a.never...) {
				if _, has := env[name]; has {
					t.Errorf("%v: %s is set; the chart never sets it", args, name)
				}
			}
		}
	})
}

// TestAddOnPostgresRunsSeveralReplicas: with Postgres the add-on runs two replicas on a rolling update, with a budget that keeps one, and no
// volume; with SQLite there is the claim, kept on uninstall, and no budget.
func TestAddOnPostgresRunsSeveralReplicas(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		name := a.objectName()
		objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.pg))
		d, _ := a.deployment(t, objs)
		ru := d.Spec.Strategy.RollingUpdate
		if *d.Spec.Replicas != 2 || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil || ru.MaxSurge.IntValue() != 1 || ru.MaxUnavailable.IntValue() != 0 {
			t.Errorf("replicas %d, strategy %+v; want two, rolled one surge at a time with none unavailable", *d.Spec.Replicas, d.Spec.Strategy)
		}
		var pdb policyv1.PodDisruptionBudget
		find(t, objs, "PodDisruptionBudget", name, &pdb)
		if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 || !maps.Equal(pdb.Spec.Selector.MatchLabels, d.Spec.Selector.MatchLabels) {
			t.Errorf("budget %+v; want maxUnavailable 1 over the add-on's pods", pdb.Spec)
		}
		for _, o := range objs {
			if o.kind() == "PersistentVolumeClaim" && strings.HasPrefix(o.name(), name) {
				t.Errorf("a claim for the add-on with Postgres: %s", o.name())
			}
		}
		if slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) {
			t.Errorf("volumes %+v; no volume with Postgres", d.Spec.Template.Spec.Volumes)
		}

		objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.sqlite))
		var pvc corev1.PersistentVolumeClaim
		find(t, objs, "PersistentVolumeClaim", name+"-data", &pvc)
		if pvc.Annotations["helm.sh/resource-policy"] != "keep" || !slices.Equal(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
			t.Errorf("claim annotations %v, access modes %v; want ReadWriteOnce, kept on uninstall", pvc.Annotations, pvc.Spec.AccessModes)
		}
		if slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "PodDisruptionBudget" }) {
			t.Errorf("a budget at one replica: %v", kinds(objs))
		}
		objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.sqlite), "--set", "addons."+a.name+".persistence.existingClaim=mine")
		d, _ = a.deployment(t, objs)
		if slices.ContainsFunc(objs, func(o object) bool {
			return o.kind() == "PersistentVolumeClaim" && strings.HasPrefix(o.name(), name)
		}) ||
			!slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
				return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "mine"
			}) {
			t.Errorf("with existingClaim: objects %v, volumes %+v; want the operator's claim and none rendered", kinds(objs), d.Spec.Template.Spec.Volumes)
		}
	})
}

// TestAddOnSelectorsNeverMatchTheOtherSide: no selector of Agent Kourier's (Deployment, Services, PodDisruptionBudget, NetworkPolicy,
// ServiceMonitor) matches the add-on's pods or Service, and none of the add-on's matches Agent Kourier's; and nothing of the add-on carries
// app.kubernetes.io/name=agent-kourier, which NetworkPolicies elsewhere trust.
func TestAddOnSelectorsNeverMatchTheOtherSide(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		name := a.objectName()
		objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.pg), "--set", "replicaCount=2,store.type=postgres,store.postgres.dsnSecret.name=pg,store.postgres.dsnSecret.key=dsn,podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=1,chatListener.enabled=true",
			"--set-json", `networkPolicy.postgres.to=[{"ipBlock":{"cidr":"10.1.0.0/16"}}]`, "--set-json", `networkPolicy.chatFrom=[{"ipBlock":{"cidr":"10.2.0.0/16"}}]`)
		var broker appsv1.Deployment
		find(t, objs, "Deployment", "agent-kourier", &broker)
		addon, _ := a.deployment(t, objs)
		var brokerSvc, addonSvc corev1.Service
		find(t, objs, "Service", "agent-kourier", &brokerSvc)
		find(t, objs, "Service", name, &addonSvc)
		// The pods as they run: the leader label is on the leader's pod, which the chat Service selects.
		brokerPod := maps.Clone(broker.Spec.Template.Labels)
		brokerPod["agentkourier.dev/role"] = "leader"
		addonPod := addon.Spec.Template.Labels

		matches := func(selector, labels map[string]string) bool {
			if len(selector) == 0 {
				return false
			}
			for k, v := range selector {
				if labels[k] != v {
					return false
				}
			}
			return true
		}
		if addonPod["app.kubernetes.io/name"] != name || addonPod["app.kubernetes.io/component"] != a.name+"-addon" {
			t.Errorf("add-on pod labels %v; want app.kubernetes.io/name %s and component %s-addon", addonPod, name, a.name)
		}
		checked := 0
		for _, o := range objs {
			if slices.Contains(o.labelValues(), name) != strings.HasPrefix(o.name(), name) {
				t.Errorf("%s %s: labelled as the add-on's %v, named as the %v", o.kind(), o.name(), slices.Contains(o.labelValues(), name), strings.HasPrefix(o.name(), name))
			}
			if strings.HasPrefix(o.name(), name) && str(sub(sub(o, "metadata"), "labels")["app.kubernetes.io/name"]) == "agent-kourier" {
				t.Errorf("%s %s carries app.kubernetes.io/name=agent-kourier", o.kind(), o.name())
			}
			var selector map[string]string
			var targets [2]map[string]string // Agent Kourier's, the add-on's
			switch o.kind() {
			case "Deployment":
				var d appsv1.Deployment
				find(t, []object{o}, "Deployment", o.name(), &d)
				selector, targets = d.Spec.Selector.MatchLabels, [2]map[string]string{brokerPod, addonPod}
			case "Service":
				var s corev1.Service
				find(t, []object{o}, "Service", o.name(), &s)
				selector, targets = s.Spec.Selector, [2]map[string]string{brokerPod, addonPod}
			case "PodDisruptionBudget":
				var p policyv1.PodDisruptionBudget
				find(t, []object{o}, "PodDisruptionBudget", o.name(), &p)
				selector, targets = p.Spec.Selector.MatchLabels, [2]map[string]string{brokerPod, addonPod}
			case "NetworkPolicy":
				var n networkingv1.NetworkPolicy
				find(t, []object{o}, "NetworkPolicy", o.name(), &n)
				selector, targets = n.Spec.PodSelector.MatchLabels, [2]map[string]string{brokerPod, addonPod}
			case "ServiceMonitor":
				var m struct {
					Spec struct {
						Selector struct {
							MatchLabels map[string]string `json:"matchLabels"`
						} `json:"selector"`
					} `json:"spec"`
				}
				find(t, []object{o}, "ServiceMonitor", o.name(), &m)
				selector, targets = m.Spec.Selector.MatchLabels, [2]map[string]string{brokerSvc.Labels, addonSvc.Labels}
			default:
				continue
			}
			checked++
			own, other := targets[0], targets[1]
			if strings.HasPrefix(o.name(), name) {
				own, other = other, own
			}
			if !matches(selector, own) {
				t.Errorf("%s %s selects %v, which does not match its own side %v", o.kind(), o.name(), selector, own)
			}
			if matches(selector, other) {
				t.Errorf("%s %s selects %v, which matches the other side %v", o.kind(), o.name(), selector, other)
			}
		}
		// Agent Kourier's Deployment, Services (metrics and chat), budget, policy and monitor, and the add-on's.
		if checked != 11 {
			t.Errorf("checked %d selectors; want 11: %v", checked, kinds(objs))
		}
	})
}

// TestAddOnNetworkPolicies: the add-on admits Agent Kourier and the scrapers on its port, and with Postgres its other replicas; it reaches DNS,
// its backend on 443, and with Postgres its other replicas, its database and what extraEgress adds. Agent Kourier's policy reaches the
// add-on's pods on its port.
func TestAddOnNetworkPolicies(t *testing.T) {
	t.Parallel()
	brokerPods := map[string]string{"app.kubernetes.io/name": "agent-kourier", "app.kubernetes.io/instance": "agent-kourier"}
	podPeer := func(peers []networkingv1.NetworkPolicyPeer, labels map[string]string) bool {
		return len(peers) == 1 && peers[0].PodSelector != nil && peers[0].NamespaceSelector == nil && maps.Equal(peers[0].PodSelector.MatchLabels, labels)
	}
	ports := func(rules []networkingv1.NetworkPolicyEgressRule) []int {
		var out []int
		for _, r := range rules {
			for _, p := range r.Ports {
				out = append(out, p.Port.IntValue())
			}
		}
		return out
	}
	eachAddOn(t, func(t *testing.T, a addOn) {
		addonPods := map[string]string{"app.kubernetes.io/name": a.objectName(), "app.kubernetes.io/instance": "agent-kourier"}
		for _, tc := range a.policies {
			t.Run(tc.values, func(t *testing.T) {
				t.Parallel()
				objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(tc.values))
				var np networkingv1.NetworkPolicy
				find(t, objs, "NetworkPolicy", a.objectName(), &np)
				if !maps.Equal(np.Spec.PodSelector.MatchLabels, addonPods) || !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{"Ingress", "Egress"}) {
					t.Errorf("policy selects %v for %v; want the add-on's pods, both directions", np.Spec.PodSelector.MatchLabels, np.Spec.PolicyTypes)
				}
				for _, r := range np.Spec.Ingress {
					if len(r.Ports) != 1 || r.Ports[0].Port.IntValue() != 8090 {
						t.Errorf("ingress rule %+v; every rule is for port 8090 alone", r)
					}
				}
				if len(np.Spec.Ingress) < 1 || !podPeer(np.Spec.Ingress[0].From, brokerPods) {
					t.Errorf("ingress %+v; want Agent Kourier's pods first", np.Spec.Ingress)
				}
				self := slices.ContainsFunc(np.Spec.Ingress, func(r networkingv1.NetworkPolicyIngressRule) bool { return podPeer(r.From, addonPods) })
				if self != tc.selfIngress || len(np.Spec.Ingress) != tc.ingress {
					t.Errorf("ingress %+v; want %d rules: Agent Kourier, the scrapers where named, and the add-on's own replicas %v", np.Spec.Ingress, tc.ingress, tc.selfIngress)
				}
				if got := ports(np.Spec.Egress); !slices.Equal(got, tc.egressPorts) {
					t.Fatalf("egress ports %v; want %v", got, tc.egressPorts)
				}
				if b := np.Spec.Egress[1].To; len(b) != 1 || b[0].IPBlock == nil || b[0].IPBlock.CIDR != "0.0.0.0/0" || !slices.Contains(b[0].IPBlock.Except, "10.0.0.0/8") ||
					!slices.Contains(b[0].IPBlock.Except, "169.254.0.0/16") {
					t.Errorf("the rule for the add-on's backend %+v; want 443 to anywhere outside the private and link-local ranges", b)
				}

				var broker networkingv1.NetworkPolicy
				find(t, objs, "NetworkPolicy", "agent-kourier", &broker)
				if !slices.ContainsFunc(broker.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
					return podPeer(r.To, addonPods) && len(r.Ports) == 1 && r.Ports[0].Port.IntValue() == 8090
				}) {
					t.Errorf("Agent Kourier's egress %+v; want a rule to the add-on's pods on 8090", broker.Spec.Egress)
				}
			})
		}
	})
}

// TestAddOnsSideBySide: every add-on in one release renders apart, each with its own objects, and Agent Kourier's policy reaches each.
func TestAddOnsSideBySide(t *testing.T) {
	t.Parallel()
	helmPath(t)
	args := []string{"-f", values("minimal"), "--set", "networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"}
	for _, a := range addOns {
		args = append(args, "--set", a.set)
	}
	objs := mustRender(t, "agent-kourier", "agent-kourier", args...)
	var broker networkingv1.NetworkPolicy
	find(t, objs, "NetworkPolicy", "agent-kourier", &broker)
	for _, a := range addOns {
		for _, kind := range []string{"Deployment", "Service", "ServiceAccount", "NetworkPolicy", "PersistentVolumeClaim"} {
			if !slices.ContainsFunc(objs, func(o object) bool { return o.kind() == kind && strings.HasPrefix(o.name(), a.objectName()) }) {
				t.Errorf("no %s of %s: %v", kind, a.objectName(), kinds(objs))
			}
		}
		if !slices.ContainsFunc(broker.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
			return len(r.To) == 1 && r.To[0].PodSelector != nil && r.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] == a.objectName()
		}) {
			t.Errorf("Agent Kourier's egress %+v; want a rule to %s's pods", broker.Spec.Egress, a.objectName())
		}
	}
}

// TestAddOnServiceAndMonitor: the add-on's Service is ClusterIP on its one port, named http, selecting its pods, and with serviceMonitor.enabled
// a ServiceMonitor of its own scrapes /metrics there, with the settings of Agent Kourier's.
func TestAddOnServiceAndMonitor(t *testing.T) {
	t.Parallel()
	eachAddOn(t, func(t *testing.T, a addOn) {
		objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.pg))
		d, _ := a.deployment(t, objs)
		var svc corev1.Service
		find(t, objs, "Service", a.objectName(), &svc)
		if svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "http" || svc.Spec.Ports[0].Port != 8090 ||
			svc.Spec.Ports[0].TargetPort.String() != "http" || svc.Spec.Ports[0].NodePort != 0 || !maps.Equal(svc.Spec.Selector, d.Spec.Selector.MatchLabels) {
			t.Errorf("Service %+v; want ClusterIP, the one port http 8090 to the pod's http, selecting the add-on's pods", svc.Spec)
		}
		var sm struct {
			Metadata struct {
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Selector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"selector"`
				Endpoints []struct {
					Port, Path, Interval string
				} `json:"endpoints"`
			} `json:"spec"`
		}
		find(t, objs, "ServiceMonitor", a.objectName(), &sm)
		if sm.Metadata.Namespace != "monitoring" || sm.Metadata.Labels["release"] != "kube-prometheus-stack" || !maps.Equal(sm.Spec.Selector.MatchLabels, d.Spec.Selector.MatchLabels) ||
			len(sm.Spec.Endpoints) != 1 || sm.Spec.Endpoints[0].Port != "http" || sm.Spec.Endpoints[0].Path != "/metrics" || sm.Spec.Endpoints[0].Interval != "30s" {
			t.Errorf("ServiceMonitor %+v; want serviceMonitor's namespace, labels and interval, /metrics on the add-on's port http", sm)
		}
		if objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(a.sqlite)); slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "ServiceMonitor" }) {
			t.Errorf("a ServiceMonitor without serviceMonitor.enabled: %v", kinds(objs))
		}
	})
}
