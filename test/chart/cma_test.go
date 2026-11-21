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

// The Claude Managed Agents add-on (addons.cma): its objects are <fullname>-cma, and its pods are labelled
// app.kubernetes.io/name=agent-kourier-cma, which none of Agent Kourier's selectors match.

const cmaName = "agent-kourier-cma"

// cmaOn is the add-on with the least it needs, on the minimal install (SQLite, one replica).
var cmaOn = []string{"-f", values("minimal"), "--set",
	"addons.cma.enabled=true,addons.cma.agentId=agent_x,addons.cma.environmentId=env_x," +
		"addons.cma.anthropicApiKeySecret.name=anthropic,addons.cma.anthropicApiKeySecret.key=apiKey,addons.cma.auth.tokenSecret.name=cma-token"}

// cmaPostgres switches cmaOn to Postgres.
const cmaPostgres = "addons.cma.store.type=postgres,addons.cma.store.postgres.dsnSecret.name=cma-pg,addons.cma.store.postgres.dsnSecret.key=dsn"

// cmaContract is every variable the add-on reads that the chart may set (the add-on's settings contract): the chart renders no other,
// apart from what addons.cma.extraEnv adds.
var cmaContract = []string{
	"CMA_AGENT_ID", "CMA_ENVIRONMENT_ID", "ANTHROPIC_API_KEY", "LISTEN_ADDR", "PUBLIC_URL", "SESSION_IDLE_TTL", "TOOL_RESULT_TIMEOUT",
	"AUTH_TOKEN_FILE", "AUTH_HEADER", "STORE", "SQLITE_PATH", "POSTGRES_DSN", "TASK_RETENTION", "LEASE_DURATION", "LEASE_RENEW_DEADLINE",
	"LEASE_RETRY_PERIOD", "ADVERTISE_URL", "POD_NAME", "LOG_LEVEL",
	"POD_IP",     // the downward API's pod address, which ADVERTISE_URL refers to
	"GOMEMLIMIT", // the Go runtime's, from the memory limit
}

func cmaDeployment(t *testing.T, objs []object) (appsv1.Deployment, map[string]corev1.EnvVar) {
	t.Helper()
	var d appsv1.Deployment
	find(t, objs, "Deployment", cmaName, &d)
	env := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return d, env
}

// TestCMAIsOffAndInertByDefault: the block is off by default, and while it is off nothing of it is rendered or checked: the minimal install
// renders the same with the block's settings changed, with the block absent (a release upgraded with --reuse-values from a chart before
// it), and with the NetworkPolicy on, which then has no rule for the add-on.
func TestCMAIsOffAndInertByDefault(t *testing.T) {
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
			{"--set", "addons.cma.enabled=false"},
			{"--set", "addons=null"},
			{"--set", "addons.cma=null"},
			// Settings the checks would refuse if the block were on: off, they are not looked at.
			{"--set", "addons.cma.replicaCount=3,addons.cma.leaderElection.leaseDuration=20s,addons.cma.store.postgres.dsnSecret.name=pg,addons.cma.agentId=agent_x"},
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
			if strings.HasSuffix(o.name(), "-cma") || strings.Contains(strings.Join(o.labelValues(), ","), "agent-kourier-cma") {
				t.Errorf("an install without the add-on rendered %s %s", o.kind(), o.name())
			}
		}
	}
}

// labelValues are the values of an object's labels.
func (o object) labelValues() []string {
	var out []string
	for _, v := range sub(sub(o, "metadata"), "labels") {
		out = append(out, str(v))
	}
	return out
}

func TestCMARefusals(t *testing.T) {
	t.Parallel()
	helmPath(t)
	on := func(more ...string) []string { return append(slices.Clone(cmaOn), more...) }
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no agent", on("--set", "addons.cma.agentId="), "addons.cma.enabled needs addons.cma.agentId"},
		{"no environment", on("--set", "addons.cma.environmentId="), "addons.cma.enabled needs addons.cma.environmentId"},
		{"no API key Secret", on("--set", "addons.cma.anthropicApiKeySecret.name="), "addons.cma.anthropicApiKeySecret.name and addons.cma.anthropicApiKeySecret.key"},
		{"an API key Secret with no key", on("--set", "addons.cma.anthropicApiKeySecret.key="), "addons.cma.anthropicApiKeySecret.name and addons.cma.anthropicApiKeySecret.key"},
		{"no token Secret", on("--set", "addons.cma.auth.tokenSecret.name="), "addons.cma.auth.tokenSecret.name and addons.cma.auth.tokenSecret.key"},
		{"a token Secret with no key", on("--set", "addons.cma.auth.tokenSecret.key="), "addons.cma.auth.tokenSecret.name and addons.cma.auth.tokenSecret.key"},
		{"the caller check turned off", on("--set", "addons.cma.auth.disabled=true"), "additional properties 'disabled' not allowed"},
		{"two replicas on SQLite", on("--set", "addons.cma.replicaCount=2"), "addons.cma.replicaCount > 1 needs addons.cma.store.type postgres"},
		{"Postgres without a DSN Secret", on("--set", "addons.cma.store.type=postgres"), "addons.cma.store.postgres.dsnSecret.name and addons.cma.store.postgres.dsnSecret.key"},
		{"Postgres with a Secret and no key", on("--set", "addons.cma.store.type=postgres,addons.cma.store.postgres.dsnSecret.name=pg"), "addons.cma.store.postgres.dsnSecret.name and addons.cma.store.postgres.dsnSecret.key"},
		{"a DSN Secret with SQLite", on("--set", "addons.cma.store.postgres.dsnSecret.name=pg,addons.cma.store.postgres.dsnSecret.key=dsn"), "addons.cma.store.postgres.dsnSecret is for addons.cma.store.type postgres"},
		{"an election setting with SQLite", on("--set", "addons.cma.leaderElection.retryPeriod=3s"), "addons.cma.leaderElection is for addons.cma.store.type postgres"},
		{"a claim with Postgres", on("--set", cmaPostgres+",addons.cma.persistence.existingClaim=mine"), "addons.cma.persistence.existingClaim is for addons.cma.store.type sqlite"},
		{"a NetworkPolicy with Postgres and no database", on("--set", cmaPostgres+",networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"), "addons.cma.networkPolicy.postgres.to"},
		{"an unknown store", on("--set", "addons.cma.store.type=mysql"), "/addons/cma/store/type"},
		{"a duration that is not one", on("--set", "addons.cma.sessionIdleTTL=forever"), "/addons/cma/sessionIdleTTL"},
		{"an election timing that is not a duration", on("--set", cmaPostgres+",addons.cma.leaderElection.leaseDuration=soon"), "/addons/cma/leaderElection/leaseDuration"},
		{"a header name with a space", on("--set", "addons.cma.auth.header=X Token"), "/addons/cma/auth/header"},
		{"a reserved header", on("--set", "addons.cma.auth.header=Cookie"), "/addons/cma/auth/header"},
		{"a reserved header in another case", on("--set", "addons.cma.auth.header=x-user-id"), "/addons/cma/auth/header"},
		{"the longest reserved header", on("--set", "addons.cma.auth.header=X-KAGENT-INSECURE-RUNTIME-IDENTITY"), "/addons/cma/auth/header"},
		{"a malformed digest", on("--set", "addons.cma.image.digest=latest"), "/addons/cma/image/digest"},
		{"an unknown log level", on("--set", "addons.cma.logLevel=trace"), "/addons/cma/logLevel"},
		{"a DSN with a password in the add-on's values", on("--set-json", `addons.cma.extraEnv=[{"name":"X","value":"postgres://cma:hunter2@db:5432/cma"}]`), "looks like a Postgres DSN with a password"},
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

// TestCMADeploymentIsSafe: the add-on's pod has the broker's hardening, no API token, its credentials from Secrets only, the accepted tokens as a
// file the kubelet keeps current, and the probes on its one port.
func TestCMADeploymentIsSafe(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma"))
	d, env := cmaDeployment(t, objs)
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
	var sa corev1.ServiceAccount
	find(t, objs, "ServiceAccount", cmaName, &sa)
	if pod.ServiceAccountName != cmaName || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount %q, pod automount %v, ServiceAccount automount %v; want the add-on's own, with no token mounted", pod.ServiceAccountName, pod.AutomountServiceAccountToken, sa.AutomountServiceAccountToken)
	}
	if slices.ContainsFunc(objs, func(o object) bool {
		return (o.kind() == "Role" || o.kind() == "RoleBinding") && strings.Contains(o.name(), "cma")
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
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			continue
		}
		if e.Name != "ANTHROPIC_API_KEY" || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != "anthropic" || e.ValueFrom.SecretKeyRef.Key != "apiKey" {
			t.Errorf("env %s from %+v; on SQLite the one variable from a source is ANTHROPIC_API_KEY, from its Secret", e.Name, e.ValueFrom)
		}
	}

	// The tokens: a read-only Secret volume, the one key as the file AUTH_TOKEN_FILE names, never by subPath (which the kubelet does not update).
	var auth *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "auth" {
			auth = &pod.Volumes[i]
		}
	}
	if auth == nil || auth.Secret == nil || auth.Secret.SecretName != "cma-token" || len(auth.Secret.Items) != 1 || auth.Secret.Items[0].Key != "token" ||
		auth.Secret.DefaultMode == nil || *auth.Secret.DefaultMode != 0o440 {
		t.Fatalf("volume auth %+v; want the Secret cma-token's key token, readable by the pod's group alone", auth)
	}
	mount := c.VolumeMounts[slices.IndexFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "auth" })]
	if !mount.ReadOnly || mount.SubPath != "" || env["AUTH_TOKEN_FILE"].Value != mount.MountPath+"/"+auth.Secret.Items[0].Path {
		t.Errorf("mount %+v, AUTH_TOKEN_FILE %q; want the file of the read-only mount, without subPath", mount, env["AUTH_TOKEN_FILE"].Value)
	}
	if _, has := env["AUTH_DISABLED"]; has {
		t.Errorf("AUTH_DISABLED is set; the chart never turns the caller check off")
	}
}

// TestCMAImage: the add-on's image defaults to its repository at the chart's appVersion, as Agent Kourier's does, and a digest wins.
func TestCMAImage(t *testing.T) {
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
	d, _ := cmaDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", cmaOn...))
	if got, want := d.Spec.Template.Spec.Containers[0].Image, "ghcr.io/agentkourier/agent-kourier-cma:"+chart.AppVersion; got != want {
		t.Errorf("image %q; want %q", got, want)
	}
	digest := "sha256:" + strings.Repeat("cd", 32)
	d, _ = cmaDeployment(t, mustRender(t, "agent-kourier", "agent-kourier", append(slices.Clone(cmaOn), "--set", "addons.cma.image.tag=v9.9.9,addons.cma.image.digest="+digest)...))
	if got, want := d.Spec.Template.Spec.Containers[0].Image, "ghcr.io/agentkourier/agent-kourier-cma@"+digest; got != want {
		t.Errorf("image %q; want %q: a digest wins over the tag", got, want)
	}
}

// TestCMAEnvironmentIsTheSettingsContract: the chart sets the add-on's variables, and no other, from the values; an empty optional value
// renders no variable, so that the add-on's own default holds.
func TestCMAEnvironmentIsTheSettingsContract(t *testing.T) {
	t.Parallel()
	helmPath(t)
	// A release and namespace of another name: the URL follows the add-on's Service.
	objs := mustRender(t, "team", "ops", cmaOn...)
	var d appsv1.Deployment
	find(t, objs, "Deployment", "team-agent-kourier-cma", &d)
	env := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	want := map[string]string{
		"CMA_AGENT_ID": "agent_x", "CMA_ENVIRONMENT_ID": "env_x", "ANTHROPIC_API_KEY": "", "LISTEN_ADDR": ":8090",
		"PUBLIC_URL": "http://team-agent-kourier-cma.ops.svc:8090/a2a", "AUTH_TOKEN_FILE": "/etc/agent-kourier-cma/auth/tokens",
		"STORE": "sqlite", "SQLITE_PATH": "/data/cma.db", "LOG_LEVEL": "info", "GOMEMLIMIT": "214748364",
	}
	got := map[string]string{}
	for name, e := range env {
		got[name] = e.Value
	}
	if !maps.Equal(got, want) {
		t.Errorf("env %v; want %v", got, want)
	}
	var svc corev1.Service
	find(t, objs, "Service", "team-agent-kourier-cma", &svc)

	// Postgres, and every optional setting.
	objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma-postgres"))
	d, env = cmaDeployment(t, objs)
	for name := range env {
		if !slices.Contains(cmaContract, name) && !strings.HasPrefix(name, "OTEL_") {
			t.Errorf("env %s is not a setting of the add-on", name)
		}
	}
	for name, value := range map[string]string{
		"STORE": "postgres", "AUTH_HEADER": "X-Cma-Token", "SESSION_IDLE_TTL": "720h", "TOOL_RESULT_TIMEOUT": "0", "TASK_RETENTION": "168h",
		"LEASE_DURATION": "20s", "LEASE_RENEW_DEADLINE": "12s", "LEASE_RETRY_PERIOD": "3s", "LOG_LEVEL": "debug", "ADVERTISE_URL": "http://$(POD_IP):8090",
		"OTEL_TRACES_EXPORTER": "otlp",
	} {
		if env[name].Value != value {
			t.Errorf("%s %q; want %q", name, env[name].Value, value)
		}
	}
	if r := env["POSTGRES_DSN"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != "cma-pg" || r.SecretKeyRef.Key != "dsn" {
		t.Errorf("POSTGRES_DSN from %+v; want the Secret cma-pg's key dsn", r)
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
}

// TestCMAPostgresRunsSeveralReplicas: with Postgres the add-on runs two replicas on a rolling update, with a budget that keeps one, and no
// volume; with SQLite there is the claim, kept on uninstall, and no budget.
func TestCMAPostgresRunsSeveralReplicas(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma-postgres"))
	d, _ := cmaDeployment(t, objs)
	ru := d.Spec.Strategy.RollingUpdate
	if *d.Spec.Replicas != 2 || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil || ru.MaxSurge.IntValue() != 1 || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("replicas %d, strategy %+v; want two, rolled one surge at a time with none unavailable", *d.Spec.Replicas, d.Spec.Strategy)
	}
	var pdb policyv1.PodDisruptionBudget
	find(t, objs, "PodDisruptionBudget", cmaName, &pdb)
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 || !maps.Equal(pdb.Spec.Selector.MatchLabels, d.Spec.Selector.MatchLabels) {
		t.Errorf("budget %+v; want maxUnavailable 1 over the add-on's pods", pdb.Spec)
	}
	for _, o := range objs {
		if o.kind() == "PersistentVolumeClaim" && strings.HasPrefix(o.name(), cmaName) {
			t.Errorf("a claim for the add-on with Postgres: %s", o.name())
		}
	}
	if slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) {
		t.Errorf("volumes %+v; no volume with Postgres", d.Spec.Template.Spec.Volumes)
	}

	objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma"))
	var pvc corev1.PersistentVolumeClaim
	find(t, objs, "PersistentVolumeClaim", cmaName+"-data", &pvc)
	if pvc.Annotations["helm.sh/resource-policy"] != "keep" || !slices.Equal(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Errorf("claim annotations %v, access modes %v; want ReadWriteOnce, kept on uninstall", pvc.Annotations, pvc.Spec.AccessModes)
	}
	if slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "PodDisruptionBudget" }) {
		t.Errorf("a budget at one replica: %v", kinds(objs))
	}
	objs = mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma"), "--set", "addons.cma.persistence.existingClaim=mine")
	d, _ = cmaDeployment(t, objs)
	if slices.ContainsFunc(objs, func(o object) bool {
		return o.kind() == "PersistentVolumeClaim" && strings.HasPrefix(o.name(), cmaName)
	}) ||
		!slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
			return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "mine"
		}) {
		t.Errorf("with existingClaim: objects %v, volumes %+v; want the operator's claim and none rendered", kinds(objs), d.Spec.Template.Spec.Volumes)
	}
}

// TestCMASelectorsNeverMatchTheOtherSide: no selector of Agent Kourier's (Deployment, Services, PodDisruptionBudget, NetworkPolicy,
// ServiceMonitor) matches the add-on's pods or Service, and none of the add-on's matches Agent Kourier's; and nothing of the add-on carries
// app.kubernetes.io/name=agent-kourier, which NetworkPolicies elsewhere trust.
func TestCMASelectorsNeverMatchTheOtherSide(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma-postgres"), "--set", "replicaCount=2,store.type=postgres,store.postgres.dsnSecret.name=pg,store.postgres.dsnSecret.key=dsn,podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=1,chatListener.enabled=true",
		"--set-json", `networkPolicy.postgres.to=[{"ipBlock":{"cidr":"10.1.0.0/16"}}]`, "--set-json", `networkPolicy.chatFrom=[{"ipBlock":{"cidr":"10.2.0.0/16"}}]`)
	var broker appsv1.Deployment
	find(t, objs, "Deployment", "agent-kourier", &broker)
	cma, _ := cmaDeployment(t, objs)
	var brokerSvc, cmaSvc corev1.Service
	find(t, objs, "Service", "agent-kourier", &brokerSvc)
	find(t, objs, "Service", cmaName, &cmaSvc)
	// The pods as they run: the leader label is on the leader's pod, which the chat Service selects.
	brokerPod := maps.Clone(broker.Spec.Template.Labels)
	brokerPod["agentkourier.dev/role"] = "leader"
	cmaPod := cma.Spec.Template.Labels

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
	if cmaPod["app.kubernetes.io/name"] != cmaName || cmaPod["app.kubernetes.io/component"] != "cma-addon" {
		t.Errorf("add-on pod labels %v; want app.kubernetes.io/name %s and component cma-addon", cmaPod, cmaName)
	}
	checked := 0
	for _, o := range objs {
		if slices.Contains(o.labelValues(), "agent-kourier-cma") != strings.HasPrefix(o.name(), cmaName) {
			t.Errorf("%s %s: labelled as the add-on's %v, named as the %v", o.kind(), o.name(), slices.Contains(o.labelValues(), "agent-kourier-cma"), strings.HasPrefix(o.name(), cmaName))
		}
		if strings.HasPrefix(o.name(), cmaName) && str(sub(sub(o, "metadata"), "labels")["app.kubernetes.io/name"]) == "agent-kourier" {
			t.Errorf("%s %s carries app.kubernetes.io/name=agent-kourier", o.kind(), o.name())
		}
		var selector map[string]string
		var targets [2]map[string]string // Agent Kourier's, the add-on's
		switch o.kind() {
		case "Deployment":
			var d appsv1.Deployment
			find(t, []object{o}, "Deployment", o.name(), &d)
			selector, targets = d.Spec.Selector.MatchLabels, [2]map[string]string{brokerPod, cmaPod}
		case "Service":
			var s corev1.Service
			find(t, []object{o}, "Service", o.name(), &s)
			selector, targets = s.Spec.Selector, [2]map[string]string{brokerPod, cmaPod}
		case "PodDisruptionBudget":
			var p policyv1.PodDisruptionBudget
			find(t, []object{o}, "PodDisruptionBudget", o.name(), &p)
			selector, targets = p.Spec.Selector.MatchLabels, [2]map[string]string{brokerPod, cmaPod}
		case "NetworkPolicy":
			var n networkingv1.NetworkPolicy
			find(t, []object{o}, "NetworkPolicy", o.name(), &n)
			selector, targets = n.Spec.PodSelector.MatchLabels, [2]map[string]string{brokerPod, cmaPod}
		case "ServiceMonitor":
			var m struct {
				Spec struct {
					Selector struct {
						MatchLabels map[string]string `json:"matchLabels"`
					} `json:"selector"`
				} `json:"spec"`
			}
			find(t, []object{o}, "ServiceMonitor", o.name(), &m)
			selector, targets = m.Spec.Selector.MatchLabels, [2]map[string]string{brokerSvc.Labels, cmaSvc.Labels}
		default:
			continue
		}
		checked++
		own, other := targets[0], targets[1]
		if strings.HasPrefix(o.name(), cmaName) {
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

// TestCMANetworkPolicies: the add-on admits Agent Kourier and the scrapers on its port, and with Postgres its other replicas; it reaches DNS,
// the Anthropic API on 443, and with Postgres its other replicas, its database and what extraEgress adds. Agent Kourier's policy reaches the
// add-on's pods on its port.
func TestCMANetworkPolicies(t *testing.T) {
	t.Parallel()
	helmPath(t)
	brokerPods := map[string]string{"app.kubernetes.io/name": "agent-kourier", "app.kubernetes.io/instance": "agent-kourier"}
	cmaPods := map[string]string{"app.kubernetes.io/name": cmaName, "app.kubernetes.io/instance": "agent-kourier"}
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
		// Agent Kourier and the scrapers; DNS (two ports), the Anthropic API.
		{"cma", 2, false, []int{53, 53, 443}},
		// Agent Kourier and the other replicas; DNS, the Anthropic API, the other replicas, the database, the collector.
		{"cma-postgres", 2, true, []int{53, 53, 443, 8090, 5432, 4318}},
	} {
		t.Run(tc.values, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values(tc.values))
			var np networkingv1.NetworkPolicy
			find(t, objs, "NetworkPolicy", cmaName, &np)
			if !maps.Equal(np.Spec.PodSelector.MatchLabels, cmaPods) || !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{"Ingress", "Egress"}) {
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
			self := slices.ContainsFunc(np.Spec.Ingress, func(r networkingv1.NetworkPolicyIngressRule) bool { return podPeer(r.From, cmaPods) })
			if self != tc.selfIngress || len(np.Spec.Ingress) != tc.ingress {
				t.Errorf("ingress %+v; want %d rules: Agent Kourier, the scrapers where named, and the add-on's own replicas %v", np.Spec.Ingress, tc.ingress, tc.selfIngress)
			}
			if got := ports(np.Spec.Egress); !slices.Equal(got, tc.egressPorts) {
				t.Fatalf("egress ports %v; want %v", got, tc.egressPorts)
			}
			if a := np.Spec.Egress[1].To; len(a) != 1 || a[0].IPBlock == nil || a[0].IPBlock.CIDR != "0.0.0.0/0" || !slices.Contains(a[0].IPBlock.Except, "10.0.0.0/8") {
				t.Errorf("the Anthropic rule %+v; want 443 to anywhere outside the private ranges", a)
			}

			var broker networkingv1.NetworkPolicy
			find(t, objs, "NetworkPolicy", "agent-kourier", &broker)
			if !slices.ContainsFunc(broker.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
				return podPeer(r.To, cmaPods) && len(r.Ports) == 1 && r.Ports[0].Port.IntValue() == 8090
			}) {
				t.Errorf("Agent Kourier's egress %+v; want a rule to the add-on's pods on 8090", broker.Spec.Egress)
			}
		})
	}
}

// TestCMAServiceAndMonitor: the add-on's Service is ClusterIP on its one port, named http, selecting its pods, and with serviceMonitor.enabled
// a ServiceMonitor of its own scrapes /metrics there, with the settings of Agent Kourier's.
func TestCMAServiceAndMonitor(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma-postgres"))
	d, _ := cmaDeployment(t, objs)
	var svc corev1.Service
	find(t, objs, "Service", cmaName, &svc)
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
	find(t, objs, "ServiceMonitor", cmaName, &sm)
	if sm.Metadata.Namespace != "monitoring" || sm.Metadata.Labels["release"] != "kube-prometheus-stack" || !maps.Equal(sm.Spec.Selector.MatchLabels, d.Spec.Selector.MatchLabels) ||
		len(sm.Spec.Endpoints) != 1 || sm.Spec.Endpoints[0].Port != "http" || sm.Spec.Endpoints[0].Path != "/metrics" || sm.Spec.Endpoints[0].Interval != "30s" {
		t.Errorf("ServiceMonitor %+v; want serviceMonitor's namespace, labels and interval, /metrics on the add-on's port http", sm)
	}
	if objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("cma")); slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "ServiceMonitor" }) {
		t.Errorf("a ServiceMonitor without serviceMonitor.enabled: %v", kinds(objs))
	}
}
