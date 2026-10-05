// Package chart_test checks the courier Helm chart (charts/courier) by rendering it with `helm template`: golden files for
// four values files, the failures a values file must not get past, the properties that make the install safe, and parity with
// the fast kind tier's manifests (hack/kind/fast/courier), so a chart that drifts from what the tiers run fails `make test`
// and not an install.
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
	"os"
	"os/exec"
	"path/filepath"
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

const (
	chartDir = "../../charts/courier"
	fastDir  = "../../hack/kind/fast/courier/"
)

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
	for _, name := range []string{"minimal", "fast", "sandbox", "full"} {
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
	if without := mustRender(t, "courier", "courier", "-f", values("minimal")); slices.ContainsFunc(without, func(o object) bool { return o.kind() == "CronJob" }) {
		t.Error("the default install has a token-refresh CronJob; it is for an AgentBackend with a Dex identity only")
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

// TestFastTierParity renders the chart with the values that stand for the fast tier's courier and compares what it makes
// with that tier's manifests, object by object, as typed Kubernetes objects. What differs is listed here, and nothing else may:
//
//   - every chart object carries the standard labels (helm.sh/chart, app.kubernetes.io/instance, version, managed-by);
//   - the Deployment's selector and pod, and the Service's selector, carry app.kubernetes.io/instance as well as
//     app.kubernetes.io/name, which both tiers' NetworkPolicies trust alone and the pod still has.
//
// The tier's config comes from its ConfigMap (existingConfigMap), which TestRenderedConfigLoadsLikeTheTiers covers.
func TestFastTierParity(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "courier", "courier", "-f", values("fast"))
	if want := []string{
		"Deployment/courier/courier", "PersistentVolumeClaim/courier/courier-data", "Role/courier/courier-secrets",
		"RoleBinding/courier/courier-secrets", "Service/courier/courier", "ServiceAccount/courier/courier",
	}; !slices.Equal(kinds(objs), want) {
		t.Fatalf("the chart rendered %v; want %v, the tier's own objects and no more", kinds(objs), want)
	}

	// dropInstance removes the one label the chart adds to the selectors, in the typed copy of the chart's object.
	dropInstance := func(m map[string]string) { delete(m, "app.kubernetes.io/instance") }

	var gotD, wantD appsv1.Deployment
	find(t, objs, "Deployment", "courier", &gotD)
	loadFast(t, "deployment.yaml", "Deployment", "courier", &wantD)
	dropInstance(gotD.Spec.Selector.MatchLabels)
	dropInstance(gotD.Spec.Template.Labels)
	if !equality.Semantic.DeepEqual(gotD.Spec, wantD.Spec) {
		t.Errorf("Deployment spec differs from hack/kind/fast/courier/deployment.yaml:\n%s", diff(t, wantD.Spec, gotD.Spec))
	}

	var gotS, wantS corev1.ServiceAccount
	find(t, objs, "ServiceAccount", "courier", &gotS)
	loadFast(t, "rbac.yaml", "ServiceAccount", "courier", &wantS)
	if *gotS.AutomountServiceAccountToken != *wantS.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount automount %v; the tier's is %v", *gotS.AutomountServiceAccountToken, *wantS.AutomountServiceAccountToken)
	}

	var gotR, wantR rbacv1.Role
	find(t, objs, "Role", "courier-secrets", &gotR)
	loadFast(t, "rbac.yaml", "Role", "courier-secrets", &wantR)
	if !equality.Semantic.DeepEqual(gotR.Rules, wantR.Rules) {
		t.Errorf("Role rules differ:\n%s", diff(t, wantR.Rules, gotR.Rules))
	}

	var gotB, wantB rbacv1.RoleBinding
	find(t, objs, "RoleBinding", "courier-secrets", &gotB)
	loadFast(t, "rbac.yaml", "RoleBinding", "courier-secrets", &wantB)
	if !equality.Semantic.DeepEqual(gotB.RoleRef, wantB.RoleRef) || !equality.Semantic.DeepEqual(gotB.Subjects, wantB.Subjects) {
		t.Errorf("RoleBinding differs:\n%s", diff(t, wantB, gotB))
	}

	var gotP, wantP corev1.PersistentVolumeClaim
	find(t, objs, "PersistentVolumeClaim", "courier-data", &gotP)
	loadFast(t, "pvc.yaml", "PersistentVolumeClaim", "courier-data", &wantP)
	if !equality.Semantic.DeepEqual(gotP.Spec, wantP.Spec) {
		t.Errorf("PersistentVolumeClaim spec differs:\n%s", diff(t, wantP.Spec, gotP.Spec))
	}
	if _, kept := gotP.Annotations["helm.sh/resource-policy"]; kept {
		t.Error("the fast tier's volume is kept on uninstall; its values turn that off")
	}

	var gotV, wantV corev1.Service
	find(t, objs, "Service", "courier", &gotV)
	loadFast(t, "service.yaml", "Service", "courier", &wantV)
	dropInstance(gotV.Spec.Selector)
	if !equality.Semantic.DeepEqual(gotV.Spec, wantV.Spec) {
		t.Errorf("Service spec differs:\n%s", diff(t, wantV.Spec, gotV.Spec))
	}
}

// loadFast reads the object of the kind and name from one of the fast tier's manifest files into into.
func loadFast(t *testing.T, file, kind, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fastDir, file))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range decode(t, string(raw)) {
		if o.kind() == kind && o.name() == name {
			find(t, []object{o}, kind, name, into)
			return
		}
	}
	t.Fatalf("%s has no %s %s", file, kind, name)
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
