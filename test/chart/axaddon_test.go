package chart_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/yaml"
)

// The AX add-on (addons.ax), an ADK agent on Amazon Bedrock or the Anthropic API that serves A2A: its objects are <fullname>-ax, and its pods are labelled
// app.kubernetes.io/name=agent-kourier-ax, which none of Agent Kourier's selectors match. Its values files are ax-addon-*: ax-values.yaml
// is the AX kind tier's Agent Kourier, not this add-on.

const axName = "agent-kourier-ax"

// axOn is the add-on with the least it needs, on the minimal install (SQLite, one replica, AWS credentials from the SDK's default chain).
var axOn = []string{"-f", values("minimal"), "--set",
	"addons.ax.enabled=true,addons.ax.model.region=us-east-1,addons.ax.model.name=us.anthropic.claude-haiku-4-5-20251001-v1:0," +
		"addons.ax.auth.tokenSecret.name=ax-token"}

// axAnthropicOn is the add-on with the least it needs on the Anthropic API, on the minimal install.
var axAnthropicOn = []string{"-f", values("minimal"), "--set",
	"addons.ax.enabled=true,addons.ax.model.provider=anthropic,addons.ax.model.name=claude-haiku-4-5," +
		"addons.ax.anthropicApiKeySecret.name=anthropic,addons.ax.anthropicApiKeySecret.key=apiKey,addons.ax.auth.tokenSecret.name=ax-token"}

// axPostgres switches axOn to Postgres.
const axPostgres = "addons.ax.store.type=postgres,addons.ax.store.postgres.dsnSecret.name=ax-pg,addons.ax.store.postgres.dsnSecret.key=dsn"

// axContract is every variable the add-on reads that the chart may set (the add-on's settings contract): the chart renders no other,
// apart from what addons.ax.extraEnv adds.
var axContract = []string{
	"MODEL_PROVIDER", "MODEL_REGION", "MODEL_NAME", "AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_API_KEY", "LISTEN_ADDR", "PUBLIC_URL", "AUTH_TOKEN_FILE", "AUTH_HEADER",
	"STORE", "SQLITE_PATH", "POSTGRES_DSN", "TASK_RETENTION", "SESSION_IDLE_TTL", "LEASE_DURATION", "LEASE_RENEW_DEADLINE", "LEASE_RETRY_PERIOD",
	"ADVERTISE_URL", "POD_NAME", "LOG_LEVEL",
	"POD_IP",     // the downward API's pod address, which ADVERTISE_URL refers to
	"GOMEMLIMIT", // the Go runtime's, from the memory limit
}

// axNever are settings of the add-on that the chart never sets: RUNNER_URL and OIDC are the AX route's, STATE_DIR is gone (the add-on
// refuses it), AUTH_DISABLED would turn the caller check off, and OTEL_SERVICE_NAME is left to the add-on's default.
var axNever = []string{"RUNNER_URL", "OIDC_ISSUER", "OIDC_AUDIENCE", "OIDC_SUBJECT", "STATE_DIR", "AUTH_DISABLED", "OTEL_SERVICE_NAME"}

func axDeployment(t *testing.T, objs []object) (appsv1.Deployment, map[string]corev1.EnvVar) {
	t.Helper()
	var d appsv1.Deployment
	find(t, objs, "Deployment", axName, &d)
	env := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return d, env
}

// TestAXIsOffAndInertByDefault: the block is off by default, and while it is off nothing of it is rendered or checked: the minimal install
// renders the same with the block's settings changed, with the block absent (a release upgraded with --reuse-values from a chart before
// it), and with the NetworkPolicy on, which then has no rule for the add-on.
func TestAXIsOffAndInertByDefault(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, base := range [][]string{
		{"-f", values("minimal")},
		{"-f", values("minimal"), "--set", "networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"},
	} {
		want, err := render(t.Context(), t, "agent-kourier", "agent-kourier", base...)
		if err != nil {
			t.Fatal(err)
		}
		for _, more := range [][]string{
			{"--set", "addons.ax.enabled=false"},
			{"--set", "addons=null"},
			{"--set", "addons.ax=null"},
			// Settings the checks would refuse if the block were on: off, they are not looked at.
			{"--set", "addons.ax.replicaCount=3,addons.ax.leaderElection.leaseDuration=20s,addons.ax.store.postgres.dsnSecret.name=pg,addons.ax.bedrock.apiKeySecret.name=bedrock"},
			{"--set", "addons.ax.model.provider=anthropic,addons.ax.model.region=us-east-1,addons.ax.anthropicApiKeySecret.key=apiKey"},
		} {
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
			if strings.HasSuffix(o.name(), "-ax") || slices.Contains(o.labelValues(), axName) {
				t.Errorf("an install without the add-on rendered %s %s", o.kind(), o.name())
			}
		}
	}
}

func TestAXRefusals(t *testing.T) {
	t.Parallel()
	helmPath(t)
	on := func(more ...string) []string { return append(slices.Clone(axOn), more...) }
	anthropic := func(more ...string) []string { return append(slices.Clone(axAnthropicOn), more...) }
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no region", on("--set", "addons.ax.model.region="), "addons.ax.model.provider bedrock needs addons.ax.model.region"},
		{"no model", on("--set", "addons.ax.model.name="), "addons.ax.enabled needs addons.ax.model.name"},
		{"no model on the Anthropic API", anthropic("--set", "addons.ax.model.name="), "addons.ax.enabled needs addons.ax.model.name"},
		{"a provider other than Bedrock or Anthropic", on("--set", "addons.ax.model.provider=openai"), "/addons/ax/model/provider"},
		{"an empty provider", on("--set", "addons.ax.model.provider="), "/addons/ax/model/provider"},
		{"a region on the Anthropic API", anthropic("--set", "addons.ax.model.region=us-east-1"), "addons.ax.model.region is for addons.ax.model.provider bedrock"},
		{"no Anthropic key Secret", anthropic("--set", "addons.ax.anthropicApiKeySecret.name="), "addons.ax.model.provider anthropic needs addons.ax.anthropicApiKeySecret.name and addons.ax.anthropicApiKeySecret.key"},
		{"an Anthropic key Secret with no key", anthropic("--set", "addons.ax.anthropicApiKeySecret.key="), "addons.ax.model.provider anthropic needs addons.ax.anthropicApiKeySecret.name and addons.ax.anthropicApiKeySecret.key"},
		{"a Bedrock key on the Anthropic API", anthropic("--set", "addons.ax.bedrock.apiKeySecret.name=bedrock,addons.ax.bedrock.apiKeySecret.key=apiKey"), "addons.ax.bedrock.apiKeySecret is for addons.ax.model.provider bedrock"},
		{"half a Bedrock key on the Anthropic API", anthropic("--set", "addons.ax.bedrock.apiKeySecret.key=apiKey"), "addons.ax.bedrock.apiKeySecret is for addons.ax.model.provider bedrock"},
		{"an Anthropic key on Bedrock", on("--set", "addons.ax.anthropicApiKeySecret.name=anthropic,addons.ax.anthropicApiKeySecret.key=apiKey"), "addons.ax.anthropicApiKeySecret is for addons.ax.model.provider anthropic"},
		{"half an Anthropic key on Bedrock", on("--set", "addons.ax.anthropicApiKeySecret.name=anthropic"), "addons.ax.anthropicApiKeySecret is for addons.ax.model.provider anthropic"},
		{"a Bedrock key Secret with no key", on("--set", "addons.ax.bedrock.apiKeySecret.name=bedrock"), "addons.ax.bedrock.apiKeySecret needs both name and key"},
		{"a Bedrock key with no Secret", on("--set", "addons.ax.bedrock.apiKeySecret.key=apiKey"), "addons.ax.bedrock.apiKeySecret needs both name and key"},
		{"no token Secret", on("--set", "addons.ax.auth.tokenSecret.name="), "addons.ax.auth.tokenSecret.name and addons.ax.auth.tokenSecret.key"},
		{"a token Secret with no key", on("--set", "addons.ax.auth.tokenSecret.key="), "addons.ax.auth.tokenSecret.name and addons.ax.auth.tokenSecret.key"},
		{"the caller check turned off", on("--set", "addons.ax.auth.disabled=true"), "additional properties 'disabled' not allowed"},
		{"OIDC, which is the AX route's", on("--set", "addons.ax.auth.oidc.issuer=https://issuer.example.test"), "additional properties 'oidc' not allowed"},
		{"a runner, which is the AX route's", on("--set", "addons.ax.runnerURL=http://127.0.0.1:8081"), "additional properties 'runnerURL' not allowed"},
		{"the CMA add-on's Anthropic rule", on("--set", "addons.ax.networkPolicy.anthropic.cidrs={0.0.0.0/0}"), "additional properties 'anthropic' not allowed"},
		{"a rule named for Bedrock", on("--set", "addons.ax.networkPolicy.bedrock.cidrs={0.0.0.0/0}"), "additional properties 'bedrock' not allowed"},
		{"two replicas on SQLite", on("--set", "addons.ax.replicaCount=2"), "addons.ax.replicaCount > 1 needs addons.ax.store.type postgres"},
		{"Postgres without a DSN Secret", on("--set", "addons.ax.store.type=postgres"), "addons.ax.store.postgres.dsnSecret.name and addons.ax.store.postgres.dsnSecret.key"},
		{"Postgres with a Secret and no key", on("--set", "addons.ax.store.type=postgres,addons.ax.store.postgres.dsnSecret.name=pg"), "addons.ax.store.postgres.dsnSecret.name and addons.ax.store.postgres.dsnSecret.key"},
		{"a DSN Secret with SQLite", on("--set", "addons.ax.store.postgres.dsnSecret.name=pg,addons.ax.store.postgres.dsnSecret.key=dsn"), "addons.ax.store.postgres.dsnSecret is for addons.ax.store.type postgres"},
		{"an election setting with SQLite", on("--set", "addons.ax.leaderElection.retryPeriod=3s"), "addons.ax.leaderElection is for addons.ax.store.type postgres"},
		{"a claim with Postgres", on("--set", axPostgres+",addons.ax.persistence.existingClaim=mine"), "addons.ax.persistence.existingClaim is for addons.ax.store.type sqlite"},
		{"a NetworkPolicy with Postgres and no database", on("--set", axPostgres+",networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"), "addons.ax.networkPolicy.postgres.to"},
		{"an unknown store", on("--set", "addons.ax.store.type=mysql"), "/addons/ax/store/type"},
		{"a duration that is not one", on("--set", "addons.ax.sessionIdleTTL=forever"), "/addons/ax/sessionIdleTTL"},
		{"a retention that is not a duration", on("--set", "addons.ax.taskRetention=a-month"), "/addons/ax/taskRetention"},
		{"an election timing that is not a duration", on("--set", axPostgres+",addons.ax.leaderElection.leaseDuration=soon"), "/addons/ax/leaderElection/leaseDuration"},
		{"a header name with a space", on("--set", "addons.ax.auth.header=X Token"), "/addons/ax/auth/header"},
		{"a reserved header", on("--set", "addons.ax.auth.header=Cookie"), "/addons/ax/auth/header"},
		{"a reserved header in another case", on("--set", "addons.ax.auth.header=x-user-id"), "/addons/ax/auth/header"},
		{"a malformed digest", on("--set", "addons.ax.image.digest=latest"), "/addons/ax/image/digest"},
		{"an unknown log level", on("--set", "addons.ax.logLevel=trace"), "/addons/ax/logLevel"},
		{"an unknown ServiceAccount setting", on("--set", "addons.ax.serviceAccount.name=mine"), "additional properties 'name' not allowed"},
		{"a DSN with a password in the add-on's values", on("--set-json", `addons.ax.extraEnv=[{"name":"X","value":"postgres://ax:hunter2@db:5432/ax"}]`), "looks like a Postgres DSN with a password"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("render error = %v; want one containing %q", err, tc.want)
			}
		})
	}
}

// TestAXDeploymentIsSafe: the add-on's pod has the broker's hardening, no Kubernetes API token, its credentials from a Secret or the
// ServiceAccount only, only /data and /tmp writable, the accepted tokens as a file the kubelet keeps current, and the probes on its one port.
func TestAXDeploymentIsSafe(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon"))
	d, env := axDeployment(t, objs)
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
	find(t, objs, "ServiceAccount", axName, &sa)
	if pod.ServiceAccountName != axName || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount %q, pod automount %v, ServiceAccount automount %v; want the add-on's own, with no token mounted", pod.ServiceAccountName, pod.AutomountServiceAccountToken, sa.AutomountServiceAccountToken)
	}
	if slices.ContainsFunc(objs, func(o object) bool {
		return (o.kind() == "Role" || o.kind() == "RoleBinding") && strings.HasSuffix(o.name(), "-ax")
	}) {
		t.Errorf("objects %v; the add-on is granted nothing", kinds(objs))
	}
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
	// No Bedrock key in ax-addon-values.yaml: the AWS SDK's default chain finds the credentials, so on SQLite no variable comes from a source.
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("env %s from %+v; on SQLite without a Bedrock key no variable comes from a source", e.Name, e.ValueFrom)
		}
	}

	// The tokens: a read-only Secret volume, the one key as the file AUTH_TOKEN_FILE names, never by subPath (which the kubelet does not update).
	var auth *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "auth" {
			auth = &pod.Volumes[i]
		}
	}
	if auth == nil || auth.Secret == nil || auth.Secret.SecretName != "ax-token" || len(auth.Secret.Items) != 1 || auth.Secret.Items[0].Key != "token" ||
		auth.Secret.DefaultMode == nil || *auth.Secret.DefaultMode != 0o440 {
		t.Fatalf("volume auth %+v; want the Secret ax-token's key token, readable by the pod's group alone", auth)
	}
	mount := c.VolumeMounts[slices.IndexFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "auth" })]
	if !mount.ReadOnly || mount.SubPath != "" || env["AUTH_TOKEN_FILE"].Value != mount.MountPath+"/"+auth.Secret.Items[0].Path {
		t.Errorf("mount %+v, AUTH_TOKEN_FILE %q; want the file of the read-only mount, without subPath", mount, env["AUTH_TOKEN_FILE"].Value)
	}
}

// TestAXServiceAccountCarriesTheAWSIdentity: addons.ax.serviceAccount.annotations reach the add-on's ServiceAccount, where IRSA reads its
// role annotation; by default it has none.
func TestAXServiceAccountCarriesTheAWSIdentity(t *testing.T) {
	t.Parallel()
	helmPath(t)
	var sa corev1.ServiceAccount
	find(t, mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon")), "ServiceAccount", axName, &sa)
	if got, want := sa.Annotations["eks.amazonaws.com/role-arn"], "arn:aws:iam::111122223333:role/agent-kourier-ax"; got != want {
		t.Errorf("ServiceAccount annotations %v; want eks.amazonaws.com/role-arn %q", sa.Annotations, want)
	}
	sa = corev1.ServiceAccount{}
	find(t, mustRender(t, "agent-kourier", "agent-kourier", axOn...), "ServiceAccount", axName, &sa)
	if len(sa.Annotations) != 0 {
		t.Errorf("ServiceAccount annotations %v; want none by default", sa.Annotations)
	}
}

// TestAXBedrockKey: with addons.ax.bedrock.apiKeySecret the Bedrock API key reaches the add-on as AWS_BEARER_TOKEN_BEDROCK from its Secret;
// without it the variable is not rendered, and the SDK's default chain applies.
func TestAXBedrockKey(t *testing.T) {
	t.Parallel()
	helmPath(t)
	_, env := axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon-postgres")))
	if r := env["AWS_BEARER_TOKEN_BEDROCK"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != "bedrock" || r.SecretKeyRef.Key != "apiKey" ||
		env["AWS_BEARER_TOKEN_BEDROCK"].Value != "" {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK %+v; want it from the Secret bedrock's key apiKey alone", env["AWS_BEARER_TOKEN_BEDROCK"])
	}
	_, env = axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", axOn...))
	if _, has := env["AWS_BEARER_TOKEN_BEDROCK"]; has {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK is set without addons.ax.bedrock.apiKeySecret")
	}
}

// TestAXAnthropicKey: with addons.ax.model.provider anthropic the Anthropic API key reaches the add-on as ANTHROPIC_API_KEY from its Secret,
// and neither MODEL_REGION nor the Bedrock key is rendered; with bedrock, ANTHROPIC_API_KEY is not rendered.
func TestAXAnthropicKey(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, args := range [][]string{axAnthropicOn, {"-f", values("ax-addon-anthropic")}} {
		_, env := axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", args...))
		if r := env["ANTHROPIC_API_KEY"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != "anthropic" || r.SecretKeyRef.Key != "apiKey" ||
			env["ANTHROPIC_API_KEY"].Value != "" {
			t.Errorf("%v: ANTHROPIC_API_KEY %+v; want it from the Secret anthropic's key apiKey alone", args, env["ANTHROPIC_API_KEY"])
		}
		if env["MODEL_PROVIDER"].Value != "anthropic" || env["MODEL_NAME"].Value != "claude-haiku-4-5" {
			t.Errorf("%v: MODEL_PROVIDER %q, MODEL_NAME %q; want anthropic and claude-haiku-4-5", args, env["MODEL_PROVIDER"].Value, env["MODEL_NAME"].Value)
		}
		for _, name := range []string{"MODEL_REGION", "AWS_BEARER_TOKEN_BEDROCK"} {
			if _, has := env[name]; has {
				t.Errorf("%v: %s is set on the Anthropic API", args, name)
			}
		}
	}
	for _, args := range [][]string{axOn, {"-f", values("ax-addon-postgres")}} {
		_, env := axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", args...))
		if _, has := env["ANTHROPIC_API_KEY"]; has {
			t.Errorf("%v: ANTHROPIC_API_KEY is set on Bedrock", args)
		}
	}
}

// TestAXImage: the add-on's image defaults to its repository at the chart's appVersion, as Agent Kourier's does, and a digest wins.
func TestAXImage(t *testing.T) {
	t.Parallel()
	helmPath(t)
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
	d, _ := axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", axOn...))
	if got, want := d.Spec.Template.Spec.Containers[0].Image, "ghcr.io/agentkourier/agent-kourier-ax:"+chart.AppVersion; got != want {
		t.Errorf("image %q; want %q", got, want)
	}
	digest := "sha256:" + strings.Repeat("ef", 32)
	d, _ = axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", append(slices.Clone(axOn), "--set", "addons.ax.image.tag=v9.9.9,addons.ax.image.digest="+digest)...))
	if got, want := d.Spec.Template.Spec.Containers[0].Image, "ghcr.io/agentkourier/agent-kourier-ax@"+digest; got != want {
		t.Errorf("image %q; want %q: a digest wins over the tag", got, want)
	}
}

// TestAXEnvironmentIsTheSettingsContract: the chart sets the add-on's variables, and no other, from the values; an empty optional value
// renders no variable, so that the add-on's own default holds.
func TestAXEnvironmentIsTheSettingsContract(t *testing.T) {
	t.Parallel()
	helmPath(t)
	// A release and namespace of another name: the URL follows the add-on's Service.
	objs := mustRender(t, "team", "ops", axOn...)
	var d appsv1.Deployment
	find(t, objs, "Deployment", "team-agent-kourier-ax", &d)
	got := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		got[e.Name] = e.Value
	}
	want := map[string]string{
		"MODEL_PROVIDER": "bedrock", "MODEL_REGION": "us-east-1", "MODEL_NAME": "us.anthropic.claude-haiku-4-5-20251001-v1:0", "LISTEN_ADDR": ":8090",
		"PUBLIC_URL": "http://team-agent-kourier-ax.ops.svc:8090/a2a", "AUTH_TOKEN_FILE": "/etc/agent-kourier-ax/auth/tokens",
		"STORE": "sqlite", "SQLITE_PATH": "/data/ax.db", "LOG_LEVEL": "info", "GOMEMLIMIT": "429496729",
	}
	if !maps.Equal(got, want) {
		t.Errorf("env %v; want %v", got, want)
	}
	var svc corev1.Service
	find(t, objs, "Service", "team-agent-kourier-ax", &svc)

	// Postgres, and every optional setting.
	d, env := axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon-postgres")))
	for name, value := range map[string]string{
		"STORE": "postgres", "AUTH_HEADER": "X-Ax-Token", "SESSION_IDLE_TTL": "720h", "TASK_RETENTION": "0",
		"LEASE_DURATION": "20s", "LEASE_RENEW_DEADLINE": "12s", "LEASE_RETRY_PERIOD": "3s", "LOG_LEVEL": "debug", "ADVERTISE_URL": "http://$(POD_IP):8090",
		"MODEL_REGION": "eu-west-1", "MODEL_NAME": "eu.anthropic.claude-haiku-4-5-20251001-v1:0", "OTEL_TRACES_EXPORTER": "otlp",
	} {
		if env[name].Value != value {
			t.Errorf("%s %q; want %q", name, env[name].Value, value)
		}
	}
	if r := env["POSTGRES_DSN"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != "ax-pg" || r.SecretKeyRef.Key != "dsn" {
		t.Errorf("POSTGRES_DSN from %+v; want the Secret ax-pg's key dsn", r)
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
	for _, args := range [][]string{axOn, axAnthropicOn, {"-f", values("ax-addon")}, {"-f", values("ax-addon-postgres")}, {"-f", values("ax-addon-anthropic")}} {
		_, env := axDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", args...))
		for name := range env {
			if !slices.Contains(axContract, name) && !strings.HasPrefix(name, "OTEL_") {
				t.Errorf("%v: env %s is not a setting of the add-on", args, name)
			}
		}
		for _, name := range axNever {
			if _, has := env[name]; has {
				t.Errorf("%v: %s is set; the chart never sets it", args, name)
			}
		}
	}
}

// TestAXPostgresRunsSeveralReplicas: with Postgres the add-on runs two replicas on a rolling update, with a budget that keeps one, and no
// volume; with SQLite there is the claim, kept on uninstall, and no budget.
func TestAXPostgresRunsSeveralReplicas(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon-postgres"))
	d, _ := axDeployment(t, objs)
	ru := d.Spec.Strategy.RollingUpdate
	if *d.Spec.Replicas != 2 || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil || ru.MaxSurge.IntValue() != 1 || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("replicas %d, strategy %+v; want two, rolled one surge at a time with none unavailable", *d.Spec.Replicas, d.Spec.Strategy)
	}
	var pdb policyv1.PodDisruptionBudget
	find(t, objs, "PodDisruptionBudget", axName, &pdb)
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 || !maps.Equal(pdb.Spec.Selector.MatchLabels, d.Spec.Selector.MatchLabels) {
		t.Errorf("budget %+v; want maxUnavailable 1 over the add-on's pods", pdb.Spec)
	}
	for _, o := range objs {
		if o.kind() == "PersistentVolumeClaim" && strings.HasPrefix(o.name(), axName) {
			t.Errorf("a claim for the add-on with Postgres: %s", o.name())
		}
	}
	if slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) {
		t.Errorf("volumes %+v; no volume with Postgres", d.Spec.Template.Spec.Volumes)
	}

	objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon"))
	var pvc corev1.PersistentVolumeClaim
	find(t, objs, "PersistentVolumeClaim", axName+"-data", &pvc)
	if pvc.Annotations["helm.sh/resource-policy"] != "keep" || !slices.Equal(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Errorf("claim annotations %v, access modes %v; want ReadWriteOnce, kept on uninstall", pvc.Annotations, pvc.Spec.AccessModes)
	}
	if slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "PodDisruptionBudget" }) {
		t.Errorf("a budget at one replica: %v", kinds(objs))
	}
	objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon"), "--set", "addons.ax.persistence.existingClaim=mine")
	d, _ = axDeployment(t, objs)
	if slices.ContainsFunc(objs, func(o object) bool {
		return o.kind() == "PersistentVolumeClaim" && strings.HasPrefix(o.name(), axName)
	}) ||
		!slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
			return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "mine"
		}) {
		t.Errorf("with existingClaim: objects %v, volumes %+v; want the operator's claim and none rendered", kinds(objs), d.Spec.Template.Spec.Volumes)
	}
}

// TestAXSelectorsNeverMatchTheOtherSide: no selector of Agent Kourier's (Deployment, Services, PodDisruptionBudget, NetworkPolicy,
// ServiceMonitor) matches the add-on's pods or Service, and none of the add-on's matches Agent Kourier's; and nothing of the add-on carries
// app.kubernetes.io/name=agent-kourier, which NetworkPolicies elsewhere trust.
func TestAXSelectorsNeverMatchTheOtherSide(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon-postgres"), "--set", "replicaCount=2,store.type=postgres,store.postgres.dsnSecret.name=pg,store.postgres.dsnSecret.key=dsn,podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=1,chatListener.enabled=true",
		"--set-json", `networkPolicy.postgres.to=[{"ipBlock":{"cidr":"10.1.0.0/16"}}]`, "--set-json", `networkPolicy.chatFrom=[{"ipBlock":{"cidr":"10.2.0.0/16"}}]`)
	var broker appsv1.Deployment
	find(t, objs, "Deployment", "agent-kourier", &broker)
	ax, _ := axDeployment(t, objs)
	var brokerSvc, axSvc corev1.Service
	find(t, objs, "Service", "agent-kourier", &brokerSvc)
	find(t, objs, "Service", axName, &axSvc)
	// The pods as they run: the leader label is on the leader's pod, which the chat Service selects.
	brokerPod := maps.Clone(broker.Spec.Template.Labels)
	brokerPod["agentkourier.dev/role"] = "leader"
	axPod := ax.Spec.Template.Labels

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
	if axPod["app.kubernetes.io/name"] != axName || axPod["app.kubernetes.io/component"] != "ax-addon" {
		t.Errorf("add-on pod labels %v; want app.kubernetes.io/name %s and component ax-addon", axPod, axName)
	}
	checked := 0
	for _, o := range objs {
		if slices.Contains(o.labelValues(), axName) != strings.HasPrefix(o.name(), axName) {
			t.Errorf("%s %s: labelled as the add-on's %v, named as the %v", o.kind(), o.name(), slices.Contains(o.labelValues(), axName), strings.HasPrefix(o.name(), axName))
		}
		if strings.HasPrefix(o.name(), axName) && str(sub(sub(o, "metadata"), "labels")["app.kubernetes.io/name"]) == "agent-kourier" {
			t.Errorf("%s %s carries app.kubernetes.io/name=agent-kourier", o.kind(), o.name())
		}
		var selector map[string]string
		var targets [2]map[string]string // Agent Kourier's, the add-on's
		switch o.kind() {
		case "Deployment":
			var d appsv1.Deployment
			find(t, []object{o}, "Deployment", o.name(), &d)
			selector, targets = d.Spec.Selector.MatchLabels, [2]map[string]string{brokerPod, axPod}
		case "Service":
			var s corev1.Service
			find(t, []object{o}, "Service", o.name(), &s)
			selector, targets = s.Spec.Selector, [2]map[string]string{brokerPod, axPod}
		case "PodDisruptionBudget":
			var p policyv1.PodDisruptionBudget
			find(t, []object{o}, "PodDisruptionBudget", o.name(), &p)
			selector, targets = p.Spec.Selector.MatchLabels, [2]map[string]string{brokerPod, axPod}
		case "NetworkPolicy":
			var n networkingv1.NetworkPolicy
			find(t, []object{o}, "NetworkPolicy", o.name(), &n)
			selector, targets = n.Spec.PodSelector.MatchLabels, [2]map[string]string{brokerPod, axPod}
		case "ServiceMonitor":
			var m struct {
				Spec struct {
					Selector struct {
						MatchLabels map[string]string `json:"matchLabels"`
					} `json:"selector"`
				} `json:"spec"`
			}
			find(t, []object{o}, "ServiceMonitor", o.name(), &m)
			selector, targets = m.Spec.Selector.MatchLabels, [2]map[string]string{brokerSvc.Labels, axSvc.Labels}
		default:
			continue
		}
		checked++
		own, other := targets[0], targets[1]
		if strings.HasPrefix(o.name(), axName) {
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
}

// TestAXNetworkPolicies: the add-on admits Agent Kourier and the scrapers on its port, and with Postgres its other replicas; it reaches DNS,
// Bedrock on 443, and with Postgres its other replicas, its database and what extraEgress adds. Agent Kourier's policy reaches the add-on's
// pods on its port.
func TestAXNetworkPolicies(t *testing.T) {
	t.Parallel()
	helmPath(t)
	brokerPods := map[string]string{"app.kubernetes.io/name": "agent-kourier", "app.kubernetes.io/instance": "agent-kourier"}
	axPods := map[string]string{"app.kubernetes.io/name": axName, "app.kubernetes.io/instance": "agent-kourier"}
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
	for _, tc := range []struct {
		values      string
		ingress     int  // rules: Agent Kourier, then the scrapers where metricsFrom names them, then the add-on's own replicas
		selfIngress bool // the add-on's own replicas (Postgres)
		egressPorts []int
	}{
		// Agent Kourier and the scrapers; DNS (two ports), Bedrock, and from extraEgress the EKS Pod Identity Agent.
		{"ax-addon", 2, false, []int{53, 53, 443, 80}},
		// Agent Kourier and the other replicas; DNS, Bedrock, the other replicas, the database, the collector.
		{"ax-addon-postgres", 2, true, []int{53, 53, 443, 8090, 5432, 4318}},
		// Agent Kourier alone (no metricsFrom); DNS and the Anthropic API.
		{"ax-addon-anthropic", 1, false, []int{53, 53, 443}},
	} {
		t.Run(tc.values, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(tc.values))
			var np networkingv1.NetworkPolicy
			find(t, objs, "NetworkPolicy", axName, &np)
			if !maps.Equal(np.Spec.PodSelector.MatchLabels, axPods) || !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{"Ingress", "Egress"}) {
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
			self := slices.ContainsFunc(np.Spec.Ingress, func(r networkingv1.NetworkPolicyIngressRule) bool { return podPeer(r.From, axPods) })
			if self != tc.selfIngress || len(np.Spec.Ingress) != tc.ingress {
				t.Errorf("ingress %+v; want %d rules: Agent Kourier, the scrapers where named, and the add-on's own replicas %v", np.Spec.Ingress, tc.ingress, tc.selfIngress)
			}
			if got := ports(np.Spec.Egress); !slices.Equal(got, tc.egressPorts) {
				t.Fatalf("egress ports %v; want %v", got, tc.egressPorts)
			}
			if b := np.Spec.Egress[1].To; len(b) != 1 || b[0].IPBlock == nil || b[0].IPBlock.CIDR != "0.0.0.0/0" || !slices.Contains(b[0].IPBlock.Except, "10.0.0.0/8") ||
				!slices.Contains(b[0].IPBlock.Except, "169.254.0.0/16") {
				t.Errorf("the rule for the model's provider %+v; want 443 to anywhere outside the private and link-local ranges", b)
			}

			var broker networkingv1.NetworkPolicy
			find(t, objs, "NetworkPolicy", "agent-kourier", &broker)
			if !slices.ContainsFunc(broker.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
				return podPeer(r.To, axPods) && len(r.Ports) == 1 && r.Ports[0].Port.IntValue() == 8090
			}) {
				t.Errorf("Agent Kourier's egress %+v; want a rule to the add-on's pods on 8090", broker.Spec.Egress)
			}
		})
	}
}

// TestAXBesideCMA: both add-ons in one release render apart, each with its own objects, and Agent Kourier's policy reaches each.
func TestAXBesideCMA(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", append(slices.Clone(axOn), "--set", cmaOn[3],
		"--set", "networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}")...)
	for _, name := range []string{axName, cmaName} {
		for _, kind := range []string{"Deployment", "Service", "ServiceAccount", "NetworkPolicy", "PersistentVolumeClaim"} {
			if !slices.ContainsFunc(objs, func(o object) bool { return o.kind() == kind && strings.HasPrefix(o.name(), name) }) {
				t.Errorf("no %s of %s: %v", kind, name, kinds(objs))
			}
		}
	}
	var broker networkingv1.NetworkPolicy
	find(t, objs, "NetworkPolicy", "agent-kourier", &broker)
	for _, name := range []string{axName, cmaName} {
		if !slices.ContainsFunc(broker.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
			return len(r.To) == 1 && r.To[0].PodSelector != nil && r.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] == name
		}) {
			t.Errorf("Agent Kourier's egress %+v; want a rule to %s's pods", broker.Spec.Egress, name)
		}
	}
}

// TestAXServiceAndMonitor: the add-on's Service is ClusterIP on its one port, named http, selecting its pods, and with serviceMonitor.enabled
// a ServiceMonitor of its own scrapes /metrics there, with the settings of Agent Kourier's.
func TestAXServiceAndMonitor(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon-postgres"))
	d, _ := axDeployment(t, objs)
	var svc corev1.Service
	find(t, objs, "Service", axName, &svc)
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
	find(t, objs, "ServiceMonitor", axName, &sm)
	if sm.Metadata.Namespace != "monitoring" || sm.Metadata.Labels["release"] != "kube-prometheus-stack" || !maps.Equal(sm.Spec.Selector.MatchLabels, d.Spec.Selector.MatchLabels) ||
		len(sm.Spec.Endpoints) != 1 || sm.Spec.Endpoints[0].Port != "http" || sm.Spec.Endpoints[0].Path != "/metrics" || sm.Spec.Endpoints[0].Interval != "30s" {
		t.Errorf("ServiceMonitor %+v; want serviceMonitor's namespace, labels and interval, /metrics on the add-on's port http", sm)
	}
	if objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon")); slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "ServiceMonitor" }) {
		t.Errorf("a ServiceMonitor without serviceMonitor.enabled: %v", kinds(objs))
	}
}
