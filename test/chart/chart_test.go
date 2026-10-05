// Package chart_test checks the courier Helm chart (charts/courier) by rendering it with `helm template`: golden files for
// five values files, the failures a values file must not get past, the properties that make the install safe, and what the
// kind tiers' values (ci/fast, ax and sandbox) render, which hack/kind's scripts and the E2E tests rely on, so a chart that
// drifts from what the tiers run fails `make test` and not an install.
//
// The tests need the helm binary, which the kind scripts need too (they find it on PATH, as these do). Without it they skip
// with a message that says so, so `make test` stays runnable on a machine that has only Go; set COURIER_REQUIRE_HELM=1 and a
// missing helm fails them instead, so a pipeline cannot pass by skipping: .github/workflows/ci.yml sets it. Refresh the
// golden files after a
// deliberate chart change with: go test ./test/chart -update
package chart_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files from the chart's current output")

const chartDir = "../../charts/courier"

// helmPath returns the helm binary, or skips the test (fails it with COURIER_REQUIRE_HELM=1) when there is none.
func helmPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("COURIER_REQUIRE_HELM") == "1" {
			t.Fatal("helm is not on PATH, and COURIER_REQUIRE_HELM=1 says the chart tests must run")
		}
		t.Skip("helm is not on PATH: the chart tests render charts/courier with `helm template` and are skipped without it (install helm, or set COURIER_REQUIRE_HELM=1 to make this a failure)")
	}
	return path
}

// helm runs the helm binary with args and returns its standard output; on failure the error carries helm's own message.
func helm(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, helmPath(t), args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("helm %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// render is `helm template` of the chart as release in namespace, with the given extra arguments (-f, --set).
func render(ctx context.Context, t *testing.T, release, namespace string, args ...string) (string, error) {
	t.Helper()
	return helm(ctx, t, append([]string{"template", release, chartDir, "--namespace", namespace}, args...)...)
}

// mustRender is render for a values set that must work.
func mustRender(t *testing.T, release, namespace string, args ...string) []object {
	t.Helper()
	out, err := render(t.Context(), t, release, namespace, args...)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, out)
}

// object is one rendered Kubernetes object.
type object map[string]any

func (o object) kind() string { return str(o["kind"]) }
func (o object) name() string { return str(object(sub(o, "metadata"))["name"]) }
func (o object) namespace() string {
	return str(object(sub(o, "metadata"))["namespace"])
}

func str(v any) string { s, _ := v.(string); return s }

func sub(m map[string]any, key string) map[string]any { s, _ := m[key].(map[string]any); return s }

// decode splits a stream of manifests into objects, in a stable order (kind, namespace, name), so that neither helm's
// version nor its ordering of kinds moves a golden file. Comments are gone, which is the point: they are not the chart's output.
func decode(t *testing.T, manifests string) []object {
	t.Helper()
	var out []object
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifests), 4096)
	for {
		var o object
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("rendered manifests do not parse: %v", err)
		}
		if len(o) > 0 {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b object) int {
		return cmp.Or(cmp.Compare(a.kind(), b.kind()), cmp.Compare(a.namespace(), b.namespace()), cmp.Compare(a.name(), b.name()))
	})
	return out
}

// find returns the one object of the kind and name, decoded into into.
func find(t *testing.T, objs []object, kind, name string, into any) {
	t.Helper()
	for _, o := range objs {
		if o.kind() == kind && o.name() == name {
			b, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, into); err != nil {
				t.Fatalf("%s %s: %v", kind, name, err)
			}
			return
		}
	}
	t.Fatalf("the chart rendered no %s %s", kind, name)
}

// kinds lists "Kind/namespace/name" for every object.
func kinds(objs []object) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.kind()+"/"+o.namespace()+"/"+o.name())
	}
	return out
}

func values(name string) string { return filepath.Join(chartDir, "ci", name+"-values.yaml") }

func TestLint(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, name := range []string{"minimal", "fast", "ax", "sandbox", "full"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if out, err := helm(t.Context(), t, "lint", "--strict", chartDir, "-f", values(name)); err != nil {
				t.Errorf("helm lint --strict: %v\n%s", err, out)
			}
		})
	}
}

// goldenCases are the values files whose full output is checked in: the namespace and release are the ones the case's
// own header says it is installed as.
var goldenCases = []struct{ name, release, namespace string }{
	{"minimal", "courier", "courier"},
	{"fast", "courier", "courier"},
	{"ax", "courier", "courier"},
	{"sandbox", "courier-sandbox", "courier"},
	{"full", "courier", "courier-system"},
}

func TestGolden(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, tc.release, tc.namespace, "-f", values(tc.name))
			var got bytes.Buffer
			for _, o := range objs {
				b, err := yaml.Marshal(o)
				if err != nil {
					t.Fatal(err)
				}
				got.WriteString("---\n")
				got.Write(b)
			}
			golden := filepath.Join("testdata", tc.name+".golden.yaml")
			if *update {
				if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (go test ./test/chart -update writes it)", err)
			}
			if !bytes.Equal(want, got.Bytes()) {
				t.Errorf("the chart's output for %s-values.yaml differs from %s; if the change is deliberate, run: go test ./test/chart -update\n%s",
					tc.name, golden, firstDifference(string(want), got.String()))
			}
		})
	}
}

// firstDifference names the first line where two texts differ, with a line of context from each.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(w), len(g)); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d:\n  golden: %q\n  chart:  %q", i+1, wl, gl)
		}
	}
	return "no difference"
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=registry.example.test/courier"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"two replicas", []string{"--set", image, "--set", "replicaCount=2"}, "replicaCount > 1 is not supported"},
		{"no image", nil, "image.repository is required"},
		{"a config two ways", []string{"--set", image, "--set", "config.existingConfigMap=mine", "-f", values("minimal")}, "set one"},
		{"a monitor without its Service", []string{"--set", image, "--set", "serviceMonitor.enabled=true"}, "serviceMonitor.enabled needs service.enabled"},
		{"a NetworkPolicy without the API server", []string{"--set", image, "--set", "networkPolicy.enabled=true"}, "networkPolicy.kubeApiServer.cidrs"},
		{"token refresh without its target", []string{"--set", image, "--set", "tokenRefresh.enabled=true"}, "tokenRefresh.targetSecret is required"},
		{"token refresh without Dex credentials", []string{"--set", image, "--set", "tokenRefresh.enabled=true,tokenRefresh.targetSecret=t,tokenRefresh.dex.tokenURL=http://dex,tokenRefresh.dex.clientId=c"}, "tokenRefresh.dex.credentialsSecret is required"},
		{"a typo in a key", []string{"--set", image, "--set", "replicas=1"}, "specifications of the schema"},
		{"a malformed digest", []string{"--set", image, "--set", "image.digest=latest"}, "image.digest"},
		{"verbose client-go", []string{"--set", image, "--set", "klogVerbosity=8"}, "klogVerbosity"},
		{"a token refreshed where no Binding reads it", []string{"-f", values("minimal"), "--set", "tokenRefresh.enabled=true,tokenRefresh.targetSecret=other-token,tokenRefresh.dex.tokenURL=http://dex,tokenRefresh.dex.clientId=c,tokenRefresh.dex.credentialsSecret=d"}, "no Binding in namespace courier has identity.tokenSecretRef naming it"},
		{"a token refreshed in the wrong namespace", []string{"-f", values("minimal"), "--set", "tokenRefresh.enabled=true,tokenRefresh.targetSecret=courier-agent-token,tokenRefresh.targetNamespace=elsewhere,tokenRefresh.dex.tokenURL=http://dex,tokenRefresh.dex.clientId=c,tokenRefresh.dex.credentialsSecret=d"}, "no Binding in namespace elsewhere"},
		{"a token refreshed under the wrong key", []string{"-f", values("minimal"), "--set", "tokenRefresh.enabled=true,tokenRefresh.targetSecret=courier-agent-token,tokenRefresh.targetKey=other,tokenRefresh.dex.tokenURL=http://dex,tokenRefresh.dex.clientId=c,tokenRefresh.dex.credentialsSecret=d"}, "no Binding in namespace courier"},
		{"a budget that keeps the only pod", []string{"--set", image, "--set", "podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=1"}, "blocks every voluntary eviction"},
		{"a budget of 100 percent", []string{"--set", image, "--set", "podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=100%"}, "blocks every voluntary eviction"},
		{"a budget with no evictions allowed", []string{"--set", image, "--set", "podDisruptionBudget.enabled=true,podDisruptionBudget.maxUnavailable=0"}, "blocks every voluntary eviction"},
		{"a budget that says nothing", []string{"--set", image, "--set", "podDisruptionBudget.enabled=true"}, "needs minAvailable or maxUnavailable"},
		{"a budget that says both", []string{"--set", image, "--set", "podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=0,podDisruptionBudget.maxUnavailable=1"}, "not both"},
		{"a credential header with a value", []string{"--set", image, "--set-json", `config.agentBackends={"b":{"spec":{"dialect":"a2a","url":"http://x","headers":[{"name":"X-Api-Key","value":"abc"}]}}}`}, `header "X-Api-Key" looks like a credential`},
		{"a Slack bot token in the values", []string{"--set", image, "--set-json", `config.chatConnections={"c":{"spec":{"platform":"slack","note":"xoxb-0-fake"}}}`}, "looks like a Slack token"},
		{"a Slack app token in the values", []string{"--set", image, "--set-json", `extraEnv=[{"name":"X","value":"xapp-1-fake"}]`}, "looks like a Slack token"},
		{"a percentage budget that rounds up to the only pod", []string{"--set", image, "--set", "podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=50%"}, "rounds up to every pod"},
		{"a database file in another directory", []string{"--set", image, "--set", "persistence.dbFile=../etc/courier.db"}, "persistence.dbFile"},
		{"a resource with no spec", []string{"--set", image, "--set", "config.bindings.b.namespace=x"}, "spec"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "courier", "courier", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("render error = %v; want one containing %q", err, tc.want)
			}
		})
	}
}

// The secret scans look for a token's shape and a credential header's name, so names that merely contain
// those letters render.
func TestLookalikeNamesRender(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=registry.example.test/courier"
	for _, tc := range []struct{ name, setJSON string }{
		{"a Secret named taxapp-slack", `config.chatConnections={"c":{"spec":{"platform":"slack","tokenSecretRef":{"name":"taxapp-slack"}}}}`},
		{"a namespace named linuxapp-prod", `config.bindings={"b":{"namespace":"linuxapp-prod","spec":{"note":"xoxbox-prod"}}}`},
		{"a header named X-Monkey-Id", `config.agentBackends={"b":{"spec":{"dialect":"a2a","url":"http://x","headers":[{"name":"X-Monkey-Id","value":"m1"}]}}}`},
		{"the Bedrock egress header", `config.agentBackends={"b":{"spec":{"dialect":"a2a","url":"http://x","headers":[{"name":"X-Courier-Egress-Host","value":"https://bedrock-runtime.us-east-1.amazonaws.com"}]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := render(t.Context(), t, "courier", "courier", "--set", image, "--set-json", tc.setJSON); err != nil {
				t.Errorf("render: %v; want it to render", err)
			}
		})
	}
}

func TestDeploymentIsSafe(t *testing.T) {
	t.Parallel()
	helmPath(t)
	var d appsv1.Deployment
	find(t, mustRender(t, "courier", "courier", "-f", values("minimal")), "Deployment", "courier", &d)
	pod, c := d.Spec.Template.Spec, d.Spec.Template.Spec.Containers[0]

	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("replicas %d, strategy %s; want one pod and Recreate: SQLite's volume is never shared", *d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	if d.Spec.Template.Labels["app.kubernetes.io/name"] != "courier" || d.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "courier" {
		t.Errorf("pod labels %v, selector %v; want app.kubernetes.io/name=courier on both: NetworkPolicies elsewhere trust it", d.Spec.Template.Labels, d.Spec.Selector.MatchLabels)
	}
	if c.Image != "registry.example.test/courier:v0.1.0" {
		t.Errorf("image %q; want the repository and tag of the values", c.Image)
	}
	sc, psc := c.SecurityContext, pod.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
		psc == nil || !*psc.RunAsNonRoot || *psc.RunAsUser != 65532 || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("security contexts %+v and %+v; want non-root, RuntimeDefault seccomp, a read-only root, no escalation, every capability dropped", psc, sc)
	}
	if c.StartupProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.HTTPGet.Path != "/healthz" || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("probes %s, %s, %s; want /healthz for startup and liveness and /readyz for readiness (never a Slack check: ready does not mean connected)",
			c.StartupProbe.HTTPGet.Path, c.LivenessProbe.HTTPGet.Path, c.ReadinessProbe.HTTPGet.Path)
	}
	if *pod.TerminationGracePeriodSeconds != 40 || pod.HostNetwork {
		t.Errorf("grace %d s, hostNetwork %v; want 40 s (past courier's 25 s ordered shutdown) and no host network", *pod.TerminationGracePeriodSeconds, pod.HostNetwork)
	}
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Memory().IsZero() {
		t.Errorf("resources %+v; want memory requests and limits", c.Resources)
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil || strings.Contains(strings.ToLower(e.Name), "token") {
			t.Errorf("env %s: the default pod has no credentials in its environment; Secrets are read through the API", e.Name)
		}
	}
	if len(c.Args) != 0 || len(c.Command) != 0 {
		t.Errorf("command %v, args %v; cmd/courier takes none", c.Command, c.Args)
	}
}

func TestKillSwitchAndImageDigest(t *testing.T) {
	t.Parallel()
	helmPath(t)
	digest := "sha256:" + strings.Repeat("ab", 32)
	objs := mustRender(t, "courier", "courier", "-f", values("minimal"), "--set", "replicaCount=0", "--set", "image.digest="+digest)
	var d appsv1.Deployment
	find(t, objs, "Deployment", "courier", &d)
	if *d.Spec.Replicas != 0 {
		t.Errorf("replicas %d; replicaCount 0 is the kill switch", *d.Spec.Replicas)
	}
	if got, want := d.Spec.Template.Spec.Containers[0].Image, "registry.example.test/courier@"+digest; got != want {
		t.Errorf("image %q; want %q: a digest wins over the tag", got, want)
	}
}

func TestDatabaseFileIsInTheVolume(t *testing.T) {
	t.Parallel()
	helmPath(t)
	dbPath := func(args ...string) string {
		var d appsv1.Deployment
		find(t, mustRender(t, "courier", "courier", append([]string{"-f", values("minimal")}, args...)...), "Deployment", "courier", &d)
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "COURIER_DB_PATH" {
				return e.Value
			}
		}
		return ""
	}
	if got := dbPath(); got != "/data/courier.db" {
		t.Errorf("COURIER_DB_PATH %q; want /data/courier.db by default", got)
	}
	if got := dbPath("--set", "persistence.dbFile=other.db"); got != "/data/other.db" {
		t.Errorf("COURIER_DB_PATH %q; want /data/other.db with persistence.dbFile set", got)
	}
}

func TestPodLabelsCannotMoveTheSelector(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "courier", "courier", "-f", values("minimal"), "--set-json", `podLabels={"app.kubernetes.io/name":"other","team":"sre"}`)
	var d appsv1.Deployment
	find(t, objs, "Deployment", "courier", &d)
	if got := d.Spec.Template.Labels; got["app.kubernetes.io/name"] != "courier" || got["team"] != "sre" {
		t.Errorf("pod labels %v; want name=courier kept and team=sre added", got)
	}
}

func TestConfigChangeRollsThePod(t *testing.T) {
	t.Parallel()
	helmPath(t)
	checksum := func(args ...string) string {
		var d appsv1.Deployment
		find(t, mustRender(t, "courier", "courier", append([]string{"-f", values("minimal")}, args...)...), "Deployment", "courier", &d)
		return d.Spec.Template.Annotations["checksum/config"]
	}
	base := checksum()
	if base == "" {
		t.Fatal("the pod has no checksum/config annotation: a changed config would not restart it")
	}
	if got := checksum("--set", "config.bindings.sre.spec.chat.channel=C0000000002"); got == base {
		t.Error("changing a Binding left the checksum as it was")
	}
	if got := checksum("--set", "config.rollOnChange=false"); got != "" {
		t.Errorf("rollOnChange=false left the checksum %q", got)
	}
}

func TestSecretRBACFollowsTheConfig(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "courier", "courier-system", "-f", values("full"))
	var namespaces []string
	for _, o := range objs {
		if o.kind() == "Role" && o.name() == "courier-secrets" {
			var r rbacv1.Role
			find(t, []object{o}, "Role", "courier-secrets", &r)
			if len(r.Rules) != 1 || !slices.Equal(r.Rules[0].Resources, []string{"secrets"}) || !slices.Equal(r.Rules[0].Verbs, []string{"list", "watch"}) {
				t.Errorf("Role in %s: rules %+v; want list and watch on secrets and nothing else", r.Namespace, r.Rules)
			}
			namespaces = append(namespaces, r.Namespace)
		}
	}
	// The connection and the backend's header Secret are in the release namespace; the Binding's token is in sre.
	if want := []string{"courier-system", "sre"}; !slices.Equal(namespaces, want) {
		t.Errorf("Secret Roles in %v; want %v: one for each namespace that holds a Secret the config names, and no others", namespaces, want)
	}
	// Without a Secret reference in a namespace, no Role there: a Binding with no token adds nothing.
	objs = mustRender(t, "courier", "courier", "-f", values("minimal"), "--set-json",
		`config.bindings={"open":{"namespace":"open","spec":{"chat":{"channel":"C1"}}}}`)
	for _, o := range objs {
		if o.kind() == "Role" && o.namespace() == "open" {
			t.Errorf("a Role in namespace open, whose Binding names no Secret: %v", kinds(objs))
		}
	}
	if objs := mustRender(t, "courier", "courier", "-f", values("minimal"), "--set", "rbac.create=false"); slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "Role" }) {
		t.Errorf("rbac.create=false still rendered a Role: %v", kinds(objs))
	}
}

func TestTokenRefreshIsLeastPrivilege(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "courier", "courier", "-f", values("sandbox"))
	var r rbacv1.Role
	find(t, objs, "Role", "courier-token-refresh", &r)
	if len(r.Rules) != 1 || !slices.Equal(r.Rules[0].Verbs, []string{"patch"}) || !slices.Equal(r.Rules[0].ResourceNames, []string{"courier-agent-token"}) || !slices.Equal(r.Rules[0].Resources, []string{"secrets"}) {
		t.Errorf("token-refresh Role rules %+v; want patch on the one Secret courier-agent-token", r.Rules)
	}
	var cj batchv1.CronJob
	find(t, objs, "CronJob", "courier-token-refresh", &cj)
	pod := cj.Spec.JobTemplate.Spec.Template
	if pod.Labels["app.kubernetes.io/name"] == "courier" {
		t.Error("the refresh pod carries app.kubernetes.io/name=courier, which the front door's NetworkPolicy trusts")
	}
	script := strings.Join(pod.Spec.Containers[0].Command, "\n")
	for _, secret := range []string{"DEX_CLIENT_SECRET", "DEX_PASSWORD", "DEX_USERNAME"} {
		// Each is imported by curl (--variable %NAME), never expanded by the shell onto an argument list.
		if strings.Contains(script, "${"+secret+"}") || strings.Contains(script, "$"+secret) {
			t.Errorf("the script expands %s in the shell, which puts it on curl's argument list", secret)
		}
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Value != "" && (strings.Contains(e.Name, "SECRET") || strings.Contains(e.Name, "PASSWORD")) {
			t.Errorf("env %s holds a value in the manifest; it must come from a Secret", e.Name)
		}
	}
	if sc := pod.Spec.Containers[0].SecurityContext; sc == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation {
		t.Errorf("refresh container security context %+v; want read-only root and no escalation", sc)
	}
	// Hourly (the token lasts 24 hours, and a sleeping laptop swallows runs), no overlap, a bounded run; for the sandbox's Dex, which
	// patches the Secret the Binding reads.
	if cj.Spec.Schedule != "0 * * * *" || cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent || cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds == nil {
		t.Errorf("schedule %q, concurrency %s; want hourly, Forbid, and a deadline", cj.Spec.Schedule, cj.Spec.ConcurrencyPolicy)
	}
	refreshEnv := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		refreshEnv[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"DEX_TOKEN_URL": "http://dex.dex.svc.cluster.local:5556/dex/token", "DEX_CLIENT_ID": "courier-binding",
		"TARGET_URL": "https://kubernetes.default.svc/api/v1/namespaces/courier/secrets/courier-agent-token", "TARGET_KEY": "token",
	} {
		if refreshEnv[name] != want {
			t.Errorf("env %s = %q; want %q", name, refreshEnv[name], want)
		}
	}
	if without := mustRender(t, "courier", "courier", "-f", values("minimal")); slices.ContainsFunc(without, func(o object) bool { return o.kind() == "CronJob" }) {
		t.Error("the default install has a token-refresh CronJob; it is for an AgentBackend with a Dex identity only")
	}
}

// TestTokenRefreshScriptNeverPrintsTheToken: the script that mints the Binding's token and patches it into the Secret says
// nothing that holds it. Every line that mentions the token is one of three (the assignment from Dex, the shape check, and
// the encoding into the pipe to the PATCH), and every line that writes anywhere is one of three: the two fixed messages and
// that pipe. So nothing else can print, log or file the token, whatever it is written as.
func TestTokenRefreshScriptNeverPrintsTheToken(t *testing.T) {
	t.Parallel()
	helmPath(t)
	var cj batchv1.CronJob
	find(t, mustRender(t, "courier", "courier", "-f", values("sandbox")), "CronJob", "courier-token-refresh", &cj)
	script := strings.Join(cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command, "\n")
	mentionsToken := regexp.MustCompile(`\$\{?tok\b|\btok=`)
	writes := regexp.MustCompile(`\becho\b|\bprintf\b|\bcat\b|\btee\b|>&|/dev/(stdout|stderr|tty)|\s>\s|>>`)
	const (
		assign = `tok=$(curl -sf -m 20 -d grant_type=password -d "client_id=${DEX_CLIENT_ID}" --data-urlencode "scope=${DEX_SCOPE}" \`
		check  = `case "${tok}" in`
		encode = `printf '{"data":{"%s":"%s"}}' "${TARGET_KEY}" "$(printf %s "${tok}" | base64 | tr -d '\n')" |`
		fail   = `*) echo "Dex gave no ID token" >&2; exit 1 ;;`
		done   = `echo "refreshed the Binding's Dex token"`
	)
	seenToken, seenWrites := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if mentionsToken.MatchString(line) {
			if !slices.Contains([]string{assign, check, encode}, line) {
				t.Errorf("the refresh script mentions the token in %q; only the assignment, the shape check and the encoding into the PATCH may", line)
			}
			seenToken[line] = true
		}
		if writes.MatchString(line) {
			if !slices.Contains([]string{encode, fail, done}, line) {
				t.Errorf("the refresh script writes in %q; only the two fixed messages and the pipe into the PATCH may", line)
			}
			seenWrites[line] = true
		}
	}
	if len(seenToken) != 3 || len(seenWrites) != 3 {
		t.Errorf("the script mentions the token in %d lines and writes in %d; want 3 and 3 (the allowed lines have changed: update this test with them)", len(seenToken), len(seenWrites))
	}
	// curl says nothing it was not asked to: no verbose or trace output, no headers dumped.
	for _, flag := range []string{"--verbose", "--trace", "--dump-header", " -v ", " -i ", "set -x", "set -o xtrace"} {
		if strings.Contains(script, flag) {
			t.Errorf("the refresh script uses %q", flag)
		}
	}
	// The ServiceAccount's token is read from its file by curl, and is on no argument list.
	for _, want := range []string{"--variable SA@/var/run/secrets/kubernetes.io/serviceaccount/token", `--expand-header "Authorization: Bearer {{SA:trim}}"`} {
		if !strings.Contains(script, want) {
			t.Errorf("the refresh script lacks %q", want)
		}
	}
	if regexp.MustCompile(`Authorization[^\n]*[$]|\$\(cat |serviceaccount/token\)`).MatchString(script) {
		t.Error("the ServiceAccount's token is put on an argument list")
	}
}

func TestTokenRefreshPatchesTheBindingsSecret(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "courier", "courier-system", "-f", values("full"))
	var r rbacv1.Role
	find(t, objs, "Role", "courier-token-refresh", &r)
	var b rbacv1.RoleBinding
	find(t, objs, "RoleBinding", "courier-token-refresh", &b)
	if r.Namespace != "sre" || b.Namespace != "sre" || len(b.Subjects) != 1 || b.Subjects[0].Name != "courier-token-refresh" || b.Subjects[0].Namespace != "courier-system" {
		t.Errorf("Role in %q, RoleBinding in %q with subjects %+v; want both in sre (the Binding's namespace) and the Job's ServiceAccount from courier-system as the subject", r.Namespace, b.Namespace, b.Subjects)
	}
	var cj batchv1.CronJob
	find(t, objs, "CronJob", "courier-token-refresh", &cj)
	var url string
	for _, e := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "TARGET_URL" {
			url = e.Value
		}
	}
	if want := "https://kubernetes.default.svc/api/v1/namespaces/sre/secrets/courier-agent-token"; url != want {
		t.Errorf("the Job patches %q; want %q, the Secret the Binding reads", url, want)
	}
}

func TestPodDisruptionBudgetThatAllowsAnEviction(t *testing.T) {
	t.Parallel()
	helmPath(t)
	var pdb struct {
		Spec struct {
			MaxUnavailable int `json:"maxUnavailable"`
		} `json:"spec"`
	}
	find(t, mustRender(t, "courier", "courier", "-f", values("minimal"), "--set", "podDisruptionBudget.enabled=true,podDisruptionBudget.maxUnavailable=1"), "PodDisruptionBudget", "courier", &pdb)
	if pdb.Spec.MaxUnavailable != 1 {
		t.Errorf("maxUnavailable %d; want 1", pdb.Spec.MaxUnavailable)
	}
	// Scaled to zero (the kill switch), a budget that keeps a pod keeps nothing: it must not block the switch.
	mustRender(t, "courier", "courier", "-f", values("minimal"), "--set", "replicaCount=0,podDisruptionBudget.enabled=true,podDisruptionBudget.minAvailable=1")
}

func TestNetworkPolicyFencesEgress(t *testing.T) {
	t.Parallel()
	helmPath(t)
	var np struct {
		Spec struct {
			PolicyTypes []string `json:"policyTypes"`
			Ingress     []struct {
				Ports []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"ingress"`
			Egress []struct {
				Ports []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"egress"`
		} `json:"spec"`
	}
	find(t, mustRender(t, "courier", "courier-system", "-f", values("full")), "NetworkPolicy", "courier", &np)
	if !slices.Equal(np.Spec.PolicyTypes, []string{"Ingress", "Egress"}) {
		t.Errorf("policy types %v; want both, so that what is not listed is denied", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 || np.Spec.Ingress[0].Ports[0].Port != 8080 {
		t.Errorf("ingress %+v; want the metrics port from the scrapers and nothing else", np.Spec.Ingress)
	}
	var ports []int
	for _, e := range np.Spec.Egress {
		for _, p := range e.Ports {
			ports = append(ports, p.Port)
		}
	}
	// DNS (two rules on 53), the API server (443 and 6443), Slack (443) and the front door the values add (4180).
	if want := []int{53, 53, 443, 6443, 443, 4180}; !slices.Equal(ports, want) {
		t.Errorf("egress ports %v; want %v", ports, want)
	}
	// With no scrapers named, nothing may reach the pod.
	var open struct {
		Spec struct {
			Ingress []any `json:"ingress"`
		} `json:"spec"`
	}
	find(t, mustRender(t, "courier", "courier", "-f", values("minimal"), "--set", "networkPolicy.enabled=true", "--set", "networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"), "NetworkPolicy", "courier", &open)
	if len(open.Spec.Ingress) != 0 {
		t.Errorf("ingress %v with no metricsFrom; want none", open.Spec.Ingress)
	}
}

func TestExistingClaimAndConfigMapAreNotRendered(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "courier", "courier", "--set", "image.repository=courier", "--set", "config.existingConfigMap=mine", "--set", "persistence.existingClaim=mine-data")
	for _, o := range objs {
		if o.kind() == "ConfigMap" || o.kind() == "PersistentVolumeClaim" {
			t.Errorf("rendered %s %s, which the values say the operator manages", o.kind(), o.name())
		}
	}
	var d appsv1.Deployment
	find(t, objs, "Deployment", "courier", &d)
	vols := map[string]corev1.Volume{}
	for _, v := range d.Spec.Template.Spec.Volumes {
		vols[v.Name] = v
	}
	if vols["config"].ConfigMap.Name != "mine" || vols["data"].PersistentVolumeClaim.ClaimName != "mine-data" {
		t.Errorf("volumes %+v; want the operator's ConfigMap and claim", vols)
	}
}

// tierCases are the kind tiers' courier as hack/kind's scripts install it and the E2E tests find it: the values, the release the
// script names, and what the chart must make of them. Everything here is something a script or a test relies on: the object
// names (kubectl -n courier get deploy courier, the runner's Role for the Deployment courier), the pod label
// app.kubernetes.io/name=courier (NetworkPolicies trust it) and app.kubernetes.io/instance=<release> (lib.sh tells the tier's
// courier from the sandbox's by it), the container name, the image the scripts load, the ConfigMap the scripts make, and
// the volume.
var tierCases = []struct {
	name, values, release string
	image, pullPolicy     string
	configMap, claim      string
	dbPath                string
	slackAPIOverride      bool     // the testhooks image's COURIER_SLACK_API_URL: the fake Slack
	objects               []string // Kind/namespace/name of every object
}{
	{"fast", "fast", "courier", "courier-e2e/courier-testhooks:dev", "Never", "courier-config", "courier-data", "/data/courier.db", true, []string{
		"Deployment/courier/courier", "PersistentVolumeClaim/courier/courier-data", "Role/courier/courier-secrets",
		"RoleBinding/courier/courier-secrets", "Service/courier/courier", "ServiceAccount/courier/courier"}},
	{"ax", "ax", "courier", "courier-e2e/courier-testhooks:dev", "Never", "courier-config", "courier-data", "/data/courier-ax.db", true, []string{
		"Deployment/courier/courier", "PersistentVolumeClaim/courier/courier-data", "Role/courier/courier-secrets",
		"RoleBinding/courier/courier-secrets", "Service/courier/courier", "ServiceAccount/courier/courier"}},
	{"sandbox", "sandbox", "courier-sandbox", "courier:dev", "Never", "courier-sandbox-config", "courier-sandbox-data", "/data/courier.db", false, []string{
		"CronJob/courier/courier-sandbox-token-refresh", "Deployment/courier/courier-sandbox", "PersistentVolumeClaim/courier/courier-sandbox-data",
		"Role/courier/courier-sandbox-secrets", "Role/courier/courier-sandbox-token-refresh", "RoleBinding/courier/courier-sandbox-secrets",
		"RoleBinding/courier/courier-sandbox-token-refresh", "ServiceAccount/courier/courier-sandbox", "ServiceAccount/courier/courier-sandbox-token-refresh"}},
}

// TestTierValuesRenderWhatTheScriptsAndTestsRely: each tier's values make the objects the tier's scripts and tests name, and
// no more (the tier's ConfigMap is its own, made by the script, and so is its Slack Secret). The full tier runs the fast
// tier's values: they differ in the config the script makes, not in the chart's output.
func TestTierValuesRenderWhatTheScriptsAndTestsRely(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range tierCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, tc.release, "courier", "-f", values(tc.values))
			if got := kinds(objs); !slices.Equal(got, tc.objects) {
				t.Fatalf("the chart rendered %v; want %v", got, tc.objects)
			}
			deployment := strings.TrimPrefix(tc.objects[slices.IndexFunc(tc.objects, func(o string) bool { return strings.HasPrefix(o, "Deployment/") })], "Deployment/courier/")
			var d appsv1.Deployment
			find(t, objs, "Deployment", deployment, &d)
			pod := d.Spec.Template.Spec
			wantLabels := map[string]string{"app.kubernetes.io/name": "courier", "app.kubernetes.io/instance": tc.release}
			if !maps.Equal(d.Spec.Selector.MatchLabels, wantLabels) || !maps.Equal(d.Spec.Template.Labels, wantLabels) {
				t.Errorf("selector %v, pod labels %v; want %v on both", d.Spec.Selector.MatchLabels, d.Spec.Template.Labels, wantLabels)
			}
			if len(pod.Containers) != 1 || pod.Containers[0].Name != "courier" {
				t.Fatalf("containers %+v; want the one, named courier (the E2E tests arm faults on it)", pod.Containers)
			}
			c := pod.Containers[0]
			if c.Image != tc.image || string(c.ImagePullPolicy) != tc.pullPolicy {
				t.Errorf("image %q with pull policy %s; want %q, %s (loaded into the cluster by the script, never pulled)", c.Image, c.ImagePullPolicy, tc.image, tc.pullPolicy)
			}
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			if env["COURIER_DB_PATH"] != tc.dbPath {
				t.Errorf("COURIER_DB_PATH %q; want %q", env["COURIER_DB_PATH"], tc.dbPath)
			}
			if _, has := env["COURIER_SLACK_API_URL"]; has != tc.slackAPIOverride || tc.slackAPIOverride && env["COURIER_SLACK_API_URL"] != "http://fakeslack.e2e.svc.cluster.local:8080/" {
				t.Errorf("COURIER_SLACK_API_URL %q (set: %v); want it set to the fake Slack only where the tier runs the testhooks image: %v", env["COURIER_SLACK_API_URL"], has, tc.slackAPIOverride)
			}
			vols := map[string]corev1.Volume{}
			for _, v := range pod.Volumes {
				vols[v.Name] = v
			}
			if vols["config"].ConfigMap == nil || vols["config"].ConfigMap.Name != tc.configMap || vols["data"].PersistentVolumeClaim == nil || vols["data"].PersistentVolumeClaim.ClaimName != tc.claim {
				t.Errorf("volumes %+v; want the ConfigMap %s and the claim %s", vols, tc.configMap, tc.claim)
			}
			var pvc corev1.PersistentVolumeClaim
			find(t, objs, "PersistentVolumeClaim", tc.claim, &pvc)
			if _, kept := pvc.Annotations["helm.sh/resource-policy"]; kept || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "standard" {
				t.Errorf("PVC annotations %v, class %v; want a volume that goes with the tier (not kept on uninstall) on kind's local-path class, standard", pvc.Annotations, pvc.Spec.StorageClassName)
			}
		})
	}
}

// TestFastTiersServiceIsTheHealthPortAndNothingPublished: the E2E runner reaches courier's health and metrics port through a
// ClusterIP Service, selecting the tier's release alone, and the Service has that one port: the Alertmanager receiver is not
// served yet, and its port must not reach the Service until it is.
func TestFastTiersServiceIsTheHealthPortAndNothingPublished(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tier := range []string{"fast", "ax"} {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, "courier", "courier", "-f", values(tier))
			var svc corev1.Service
			find(t, objs, "Service", "courier", &svc)
			var d appsv1.Deployment
			find(t, objs, "Deployment", "courier", &d)
			c := d.Spec.Template.Spec.Containers[0]
			if svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.ExternalIPs) != 0 || svc.Spec.LoadBalancerIP != "" ||
				len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "http" || svc.Spec.Ports[0].Port != 8080 || svc.Spec.Ports[0].NodePort != 0 || svc.Spec.Ports[0].TargetPort.String() != "http" {
				t.Errorf("Service %+v; want ClusterIP, nothing published, the one port http 8080 to the pod's http", svc.Spec)
			}
			if !maps.Equal(svc.Spec.Selector, d.Spec.Selector.MatchLabels) {
				t.Errorf("Service selector %v; want the Deployment's %v", svc.Spec.Selector, d.Spec.Selector.MatchLabels)
			}
			if len(c.Ports) != 1 || c.Ports[0].Name != "http" || c.Ports[0].ContainerPort != 8080 || c.Ports[0].HostPort != 0 || d.Spec.Template.Spec.HostNetwork {
				t.Errorf("container ports %+v, hostNetwork %v; want only http on 8080, never on the host", c.Ports, d.Spec.Template.Spec.HostNetwork)
			}
		})
	}
}

// diff renders two values as YAML for a failure message.
func diff(t *testing.T, want, got any) string {
	t.Helper()
	w, err1 := yaml.Marshal(want)
	g, err2 := yaml.Marshal(got)
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	return firstDifference(string(w), string(g)) + "\n--- tier\n" + string(w) + "--- chart\n" + string(g)
}

// TestRenderedConfigLoadsLikeTheTiers puts each kind tier's courier resources into chart values, renders the ConfigMap, and
// loads it with courier's own loader: the Set must equal the one loaded from the tier's files, so that anything the tiers
// run can be said in values, and the chart writes what the loader reads.
func TestRenderedConfigLoadsLikeTheTiers(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, dir := range []string{"fast", "ax"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			src := filepath.Join("../../hack/kind", dir, "courier", "config")
			want, err := config.Load(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			vals := filepath.Join(t.TempDir(), "values.json")
			writeJSON(t, vals, map[string]any{
				"image": map[string]any{"repository": "courier"},
				"config": map[string]any{
					"agentBackends":   entries(want.AgentBackends, func(o v1alpha1.AgentBackend) (string, string, any) { return o.Name, o.Namespace, o.Spec }),
					"chatConnections": entries(want.ChatConnections, func(o v1alpha1.ChatConnection) (string, string, any) { return o.Name, o.Namespace, o.Spec }),
					"bindings":        entries(want.Bindings, func(o v1alpha1.Binding) (string, string, any) { return o.Name, o.Namespace, o.Spec }),
				},
			})
			var cm corev1.ConfigMap
			find(t, mustRender(t, "courier", "courier", "-f", vals), "ConfigMap", "courier-config", &cm)

			rendered := t.TempDir()
			for name, content := range cm.Data {
				if err := os.WriteFile(filepath.Join(rendered, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := config.Load(t.Context(), rendered)
			if err != nil {
				t.Fatalf("courier's loader rejects the rendered ConfigMap: %v", err)
			}
			if len(got.Bindings) != len(want.Bindings) || len(got.ChatConnections) != len(want.ChatConnections) || len(got.AgentBackends) != len(want.AgentBackends) {
				t.Fatalf("loaded %d Bindings, %d connections and %d backends; the tier has %d, %d and %d",
					len(got.Bindings), len(got.ChatConnections), len(got.AgentBackends), len(want.Bindings), len(want.ChatConnections), len(want.AgentBackends))
			}
			byName := func(a, b interface{ GetName() string }) int { return cmp.Compare(a.GetName(), b.GetName()) }
			slices.SortFunc(got.Bindings, func(a, b v1alpha1.Binding) int { return byName(&a, &b) })
			slices.SortFunc(want.Bindings, func(a, b v1alpha1.Binding) int { return byName(&a, &b) })
			slices.SortFunc(got.AgentBackends, func(a, b v1alpha1.AgentBackend) int { return byName(&a, &b) })
			slices.SortFunc(want.AgentBackends, func(a, b v1alpha1.AgentBackend) int { return byName(&a, &b) })
			if !equality.Semantic.DeepEqual(got, want) {
				t.Errorf("the rendered config loads differently from %s:\n%s", src, diff(t, want, got))
			}
		})
	}
}

// entries turns resources into the chart's map of name to {namespace, spec}, through JSON so that the values hold exactly
// what the resource's own field names say.
func entries[T any](objs []T, parts func(T) (name, namespace string, spec any)) map[string]any {
	out := map[string]any{}
	for _, o := range objs {
		name, namespace, spec := parts(o)
		b, err := json.Marshal(spec)
		if err != nil {
			panic(err) // a typed spec always marshals; this is a test helper
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			panic(err)
		}
		out[name] = map[string]any{"namespace": namespace, "spec": m}
	}
	return out
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
