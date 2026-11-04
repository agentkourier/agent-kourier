package chart_test

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// The chart's two config routes (spec D50): files, the default, which mounts a ConfigMap, and crds, which reads Binding, AgentBackend and
// ChatConnection custom resources. These tests hold the default to what it always rendered, and the crds route to what the broker needs of it.

// crdsServed makes helm template render as if the cluster already served the CRDs on a Kubernetes that CR mode accepts: the render of
// config.source crds refuses to go on otherwise (a CRD and the resources of it cannot be in one release's first apply).
var crdsServed = []string{"--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.30.2"}

// crMode is crdsServed plus more arguments, as a new slice.
func crMode(more ...string) []string { return append(slices.Clone(crdsServed), more...) }

// deployment returns the one Deployment of a render.
func deployment(t *testing.T, objs []object) appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	for _, o := range objs {
		if o.kind() == "Deployment" {
			find(t, objs, "Deployment", o.name(), &d)
			return d
		}
	}
	t.Fatalf("no Deployment in %v", kinds(objs))
	return d
}

// podEnv is the environment of the Deployment's one container, by name.
func podEnv(d appsv1.Deployment) map[string]string {
	env := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// TestDefaultsAreTheConfigMapRoute: with config.source and crds.install at their defaults, which every values file of the repository leaves, the
// chart renders what it rendered before CR mode (the golden files say so object by object); this says what that means. No CRD, ClusterRole
// or custom resource, no environment variable of CR mode, the ConfigMap mounted where the broker reads its directory, and naming the defaults
// changes nothing, byte for byte.
func TestDefaultsAreTheConfigMapRoute(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range goldenCases {
		if tc.name == "crds" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []string{"-f", values(tc.name)}
			plain, err := render(t.Context(), t, tc.release, tc.namespace, args...)
			if err != nil {
				t.Fatal(err)
			}
			named, err := render(t.Context(), t, tc.release, tc.namespace, append(args, "--set", "config.source=files,crds.install=false")...)
			if err != nil {
				t.Fatal(err)
			}
			if plain != named {
				t.Errorf("config.source=files and crds.install=false change the render:\n%s", firstDifference(plain, named))
			}
			objs := decode(t, plain)
			for _, o := range objs {
				if o.kind() == "CustomResourceDefinition" || o.kind() == "ClusterRole" || o.kind() == "ClusterRoleBinding" || strings.HasPrefix(str(o["apiVersion"]), "agentkourier.dev/") {
					t.Errorf("the default rendered %s %s, which only the route of custom resources makes", o.kind(), o.name())
				}
			}
			d := deployment(t, objs)
			env := podEnv(d)
			for _, name := range []string{"AGENTKOURIER_CONFIG_SOURCE", "AGENTKOURIER_CHAT_CONNECTION"} {
				if _, has := env[name]; has {
					t.Errorf("the pod sets %s by default: its template would move for every install", name)
				}
			}
			if env["AGENTKOURIER_CONFIG_DIR"] != "/etc/agent-kourier" {
				t.Errorf("AGENTKOURIER_CONFIG_DIR %q; want /etc/agent-kourier", env["AGENTKOURIER_CONFIG_DIR"])
			}
			pod := d.Spec.Template.Spec
			if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "config" && v.ConfigMap != nil }) ||
				!slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool {
					return m.Name == "config" && m.MountPath == "/etc/agent-kourier" && m.ReadOnly
				}) {
				t.Errorf("volumes %+v, mounts %+v; want the config ConfigMap mounted read-only at /etc/agent-kourier", pod.Volumes, pod.Containers[0].VolumeMounts)
			}
		})
	}
}

// TestCRDsInstallAddsOnlyTheCRDs: crds.install on the files route is the first step of a migration, and the CRDs are inert there: three
// CustomResourceDefinitions and nothing else, and not a byte of the Deployment moves.
func TestCRDsInstallAddsOnlyTheCRDs(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, name := range []string{"minimal", "full", "postgres"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ns := "agent-kourier"
			if name == "full" {
				ns = "agent-kourier-system"
			}
			base := mustRender(t, "agent-kourier", ns, "-f", values(name))
			with := mustRender(t, "agent-kourier", ns, "-f", values(name), "--set", "crds.install=true")
			var crds, rest []object
			for _, o := range with {
				if o.kind() == "CustomResourceDefinition" {
					crds = append(crds, o)
				} else {
					rest = append(rest, o)
				}
			}
			var names []string
			for _, o := range crds {
				names = append(names, o.name())
			}
			if want := []string{"agentbackends.agentkourier.dev", "bindings.agentkourier.dev", "chatconnections.agentkourier.dev"}; !slices.Equal(names, want) {
				t.Errorf("CRDs %v; want %v", names, want)
			}
			if !equality.Semantic.DeepEqual(base, rest) {
				t.Errorf("crds.install changed more than the CRDs:\n%s", diff(t, base, rest))
			}
		})
	}
}

// TestCRDsAreTheGeneratedFiles: the chart's CRDs are the files that `make crds` generates, rendered, not a second copy that can drift: the
// spec of each is the file's, byte for byte (the file is not parsed and written again), with the chart's labels, resource-policy keep and the
// SHA-256 of the generated file added to the metadata.
func TestCRDsAreTheGeneratedFiles(t *testing.T) {
	t.Parallel()
	helmPath(t)
	out, err := render(t.Context(), t, "agent-kourier", "agent-kourier", "-f", values("minimal"), "--set", "crds.install=true")
	if err != nil {
		t.Fatal(err)
	}
	objs := decode(t, out)
	files, err := filepath.Glob(filepath.Join(chartDir, "files", "crds", "*.yaml"))
	if err != nil || len(files) != 3 {
		t.Fatalf("the generated CRDs are %v (%v); want three", files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var want object
			if err := yaml.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			var got object
			find(t, objs, "CustomResourceDefinition", want.name(), &got)
			if !equality.Semantic.DeepEqual(got["spec"], want["spec"]) {
				t.Errorf("the rendered spec of %s differs from %s", want.name(), f)
			}
			// The text, not only the parsed form: a number that was parsed and written again can change its form.
			specText := strings.TrimSpace(string(raw)[strings.Index(string(raw), "\nspec:\n")+1:])
			if !strings.Contains(out, specText) {
				t.Errorf("the rendered %s does not hold the text of %s from `spec:` on, as it is", want.name(), f)
			}
			meta := sub(got, "metadata")
			ann := sub(meta, "annotations")
			if ann["helm.sh/resource-policy"] != "keep" {
				t.Errorf("annotations %v; want helm.sh/resource-policy keep, so that an uninstall leaves the CRD and every resource of it", ann)
			}
			if ann["agentkourier.dev/schema-sha256"] != sha256Hex(raw) {
				t.Errorf("schema-sha256 %v; want the SHA-256 of %s, %s", ann["agentkourier.dev/schema-sha256"], f, sha256Hex(raw))
			}
			if ann["controller-gen.kubebuilder.io/version"] == nil {
				t.Errorf("annotations %v lost what controller-gen wrote", ann)
			}
			if labels := sub(meta, "labels"); labels["app.kubernetes.io/managed-by"] != "Helm" || labels["app.kubernetes.io/name"] != "agent-kourier" {
				t.Errorf("labels %v; want the chart's", labels)
			}
		})
	}
}

// TestCRMode is the golden file of ci/crds-values.yaml, said in assertions: what the route of custom resources renders, and what the broker
// needs of it. The golden pins every field; this names the ones the install depends on.
func TestCRMode(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", append([]string{"-f", values("crds")}, crdsServed...)...)

	wantKinds := []string{
		"AgentBackend/agent-kourier/agents", "Binding/sre/sre", "ChatConnection/agent-kourier/slack-main",
		"ClusterRole//agent-kourier-agent-kourier-crs", "ClusterRoleBinding//agent-kourier-agent-kourier-crs",
		"Deployment/agent-kourier/agent-kourier", "PersistentVolumeClaim/agent-kourier/agent-kourier-data",
		"Role/agent-kourier/agent-kourier-secrets", "Role/platform/agent-kourier-secrets", "Role/sre/agent-kourier-secrets", "Role/team-b/agent-kourier-secrets",
		"RoleBinding/agent-kourier/agent-kourier-secrets", "RoleBinding/platform/agent-kourier-secrets", "RoleBinding/sre/agent-kourier-secrets", "RoleBinding/team-b/agent-kourier-secrets",
		"ServiceAccount/agent-kourier/agent-kourier",
	}
	if got := kinds(objs); !slices.Equal(got, wantKinds) {
		t.Errorf("objects\n%v\nwant\n%v: custom resources and no ConfigMap, no CRDs (crds.install is off), a ClusterRole, and a Role of each namespace that holds a Secret", got, wantKinds)
	}

	d := deployment(t, objs)
	env := podEnv(d)
	if env["AGENTKOURIER_CONFIG_SOURCE"] != "crds" || env["AGENTKOURIER_CHAT_CONNECTION"] != "agent-kourier/slack-main" {
		t.Errorf("source %q, connection %q; want crds and the one ChatConnection of the values, as namespace/name", env["AGENTKOURIER_CONFIG_SOURCE"], env["AGENTKOURIER_CHAT_CONNECTION"])
	}
	if _, has := env["AGENTKOURIER_CONFIG_DIR"]; has {
		t.Error("AGENTKOURIER_CONFIG_DIR is set with crds: there is no directory to read")
	}
	pod := d.Spec.Template.Spec
	for _, v := range pod.Volumes {
		if v.Name == "config" || v.ConfigMap != nil {
			t.Errorf("volume %+v: the pod mounts no ConfigMap with crds", v)
		}
	}
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "config" {
			t.Errorf("mount %+v with crds", m)
		}
	}
	if _, has := d.Spec.Template.Annotations["checksum/config"]; has {
		t.Errorf("annotations %v: a config change must not roll the pod with crds, which reloads custom resources in place", d.Spec.Template.Annotations)
	}

	// The custom resources are the values' resources, with the chart's labels, and the broker accepts each as the file loader would.
	raw, err := os.ReadFile(values("crds"))
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Namespace string
		Spec      map[string]any
	}
	var vals struct {
		Config struct{ AgentBackends, ChatConnections, Bindings map[string]entry }
	}
	if err := yaml.Unmarshal(raw, &vals); err != nil {
		t.Fatal(err)
	}
	for kind, resources := range map[string]map[string]entry{"AgentBackend": vals.Config.AgentBackends, "ChatConnection": vals.Config.ChatConnections, "Binding": vals.Config.Bindings} {
		if len(resources) == 0 {
			t.Errorf("the values have no %s: the test proves nothing about it", kind)
		}
		for name, want := range resources {
			var got object
			find(t, objs, kind, name, &got)
			if got["apiVersion"] != "agentkourier.dev/v1alpha1" {
				t.Errorf("%s %s apiVersion %v", kind, name, got["apiVersion"])
			}
			ns := cmp.Or(want.Namespace, "agent-kourier")
			if got.namespace() != ns {
				t.Errorf("%s %s namespace %q; want %q", kind, name, got.namespace(), ns)
			}
			if labels := sub(sub(got, "metadata"), "labels"); labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("%s %s labels %v; want the chart's", kind, name, labels)
			}
			if !equality.Semantic.DeepEqual(got["spec"], any(want.Spec)) {
				t.Errorf("%s %s spec differs from the values:\n%s", kind, name, diff(t, want.Spec, got["spec"]))
			}
			if errs := decodeCR(t, kind, got); len(errs) > 0 {
				t.Errorf("%s %s does not pass the broker's Default and Validate: %v", kind, name, errs)
			}
		}
	}

	// What the broker does to the custom resources, and no more.
	var cr rbacv1.ClusterRole
	find(t, objs, "ClusterRole", "agent-kourier-agent-kourier-crs", &cr)
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"agentkourier.dev"}, Resources: []string{"agentbackends", "bindings", "chatconnections"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"agentkourier.dev"}, Resources: []string{"bindings/status"}, Verbs: []string{"patch"}},
		{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"},
			ResourceNames: []string{"agentbackends.agentkourier.dev", "bindings.agentkourier.dev", "chatconnections.agentkourier.dev"}, Verbs: []string{"get"}},
	}
	if !equality.Semantic.DeepEqual(cr.Rules, wantRules) {
		t.Errorf("ClusterRole rules:\n%s", diff(t, wantRules, cr.Rules))
	}
	var crb rbacv1.ClusterRoleBinding
	find(t, objs, "ClusterRoleBinding", "agent-kourier-agent-kourier-crs", &crb)
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != cr.Name || len(crb.Subjects) != 1 ||
		crb.Subjects[0].Kind != "ServiceAccount" || crb.Subjects[0].Name != "agent-kourier" || crb.Subjects[0].Namespace != "agent-kourier" {
		t.Errorf("ClusterRoleBinding %+v; want the ClusterRole bound to the release's ServiceAccount alone", crb)
	}

	// The Secret Roles name the Secrets the chart can see, by namespace; a namespace of rbac.secretNamespaces is the one that is whole.
	wantNames := map[string][]string{
		"agent-kourier": {"agents-team", "slack-main"}, // the backend's header and the connection's credentials
		"platform":      {"shared-token"},              // rbac.secretNames
		"sre":           {"agent-kourier-agent-token"}, // the Binding's token, in the Binding's namespace
		"team-b":        nil,                           // rbac.secretNamespaces: every Secret of the namespace
	}
	for ns, want := range wantNames {
		var r rbacv1.Role
		var one []object
		for _, o := range objs {
			if o.kind() == "Role" && o.namespace() == ns && o.name() == "agent-kourier-secrets" {
				one = append(one, o)
			}
		}
		find(t, one, "Role", "agent-kourier-secrets", &r)
		wantRule := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: want, Verbs: []string{"list", "watch"}}}
		if !equality.Semantic.DeepEqual(r.Rules, wantRule) {
			t.Errorf("Role in %s:\n%s", ns, diff(t, wantRule, r.Rules))
		}
	}
}

// sha256Hex is the hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// decodeCR passes a rendered custom resource through the decoder of the file loader, which the broker's CR source uses too.
func decodeCR(t *testing.T, kind string, o object) []error {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	switch kind {
	case "AgentBackend":
		_, errs = config.DecodeObject[v1alpha1.AgentBackend](b)
	case "ChatConnection":
		_, errs = config.DecodeObject[v1alpha1.ChatConnection](b)
	case "Binding":
		_, errs = config.DecodeObject[v1alpha1.Binding](b)
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	return errs
}

// TestCRModeRendersWhatTheLoaderReads puts each kind tier's resources into values, renders them as custom resources, and decodes each as the
// broker's CR source does: the objects must equal the ones the loader reads from the tier's files, so that anything the file route runs can
// be said as custom resources by the chart.
func TestCRModeRendersWhatTheLoaderReads(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, dir := range []string{"fast", "ax"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			want, err := config.Load(t.Context(), filepath.Join("../../hack/kind", dir, "agent-kourier", "config"))
			if err != nil {
				t.Fatal(err)
			}
			vals := filepath.Join(t.TempDir(), "values.json")
			writeJSON(t, vals, map[string]any{
				"image": map[string]any{"repository": "agent-kourier"},
				"config": map[string]any{
					"source":          "crds",
					"agentBackends":   entries(want.AgentBackends, func(o v1alpha1.AgentBackend) (string, string, any) { return o.Name, o.Namespace, o.Spec }),
					"chatConnections": entries(want.ChatConnections, func(o v1alpha1.ChatConnection) (string, string, any) { return o.Name, o.Namespace, o.Spec }),
					"bindings":        entries(want.Bindings, func(o v1alpha1.Binding) (string, string, any) { return o.Name, o.Namespace, o.Spec }),
				},
			})
			objs := mustRender(t, "agent-kourier", "agent-kourier", append([]string{"-f", vals}, crdsServed...)...)

			got := map[string]any{}
			for _, o := range objs {
				switch o.kind() {
				case "AgentBackend", "ChatConnection", "Binding":
					if errs := decodeCR(t, o.kind(), o); len(errs) > 0 {
						t.Fatalf("%s %s: %v", o.kind(), o.name(), errs)
					}
					got[o.kind()+"/"+o.namespace()+"/"+o.name()] = o["spec"]
				}
			}
			wantSpecs := map[string]any{}
			for _, b := range want.AgentBackends {
				wantSpecs["AgentBackend/"+b.Namespace+"/"+b.Name] = specJSON(t, b.Spec)
			}
			for _, c := range want.ChatConnections {
				wantSpecs["ChatConnection/"+c.Namespace+"/"+c.Name] = specJSON(t, c.Spec)
			}
			for _, b := range want.Bindings {
				wantSpecs["Binding/"+b.Namespace+"/"+b.Name] = specJSON(t, b.Spec)
			}
			if !equality.Semantic.DeepEqual(got, wantSpecs) {
				t.Errorf("the rendered custom resources differ from the tier's files:\n%s", diff(t, wantSpecs, got))
			}
		})
	}
}

// specJSON is a spec as the generic map the chart renders it as.
func specJSON(t *testing.T, spec any) any {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// secretRefs lists the Secrets a custom resource's spec names, as namespace/name pairs: every credentialsSecretRef, tokenSecretRef and
// valueSecretRef anywhere in it, in the namespace of the resource that holds it.
func secretRefs(namespace string, v any, out *[]string) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if m, ok := child.(map[string]any); ok && strings.HasSuffix(k, "SecretRef") {
				if name, _ := m["name"].(string); name != "" {
					*out = append(*out, namespace+"/"+name)
				}
				continue
			}
			secretRefs(namespace, child, out)
		}
	case []any:
		for _, child := range v {
			secretRefs(namespace, child, out)
		}
	}
}

// TestCRModeSecretRolesCoverEveryReference: with crds the Roles name Secrets, so a Secret the config names and no Role names would be unreadable
// at runtime, found only in the broker's log. Every reference in the rendered custom resources, and the Postgres DSN's, is named by a Role in its
// namespace, and the namespaces that hold Roles are the ones the files route grants, whose helper is written separately.
func TestCRModeSecretRolesCoverEveryReference(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range []struct{ name, namespace string }{{"minimal", "agent-kourier"}, {"full", "agent-kourier-system"}, {"postgres", "agent-kourier"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := mustRender(t, "agent-kourier", tc.namespace, "-f", values(tc.name))
			objs := mustRender(t, "agent-kourier", tc.namespace, append([]string{"-f", values(tc.name), "--set", "config.source=crds"}, crdsServed...)...)

			var wantRefs []string
			for _, o := range objs {
				switch o.kind() {
				case "AgentBackend", "ChatConnection", "Binding":
					secretRefs(o.namespace(), o["spec"], &wantRefs)
				}
			}
			if strings.Contains(tc.name, "postgres") {
				// The DSN's Secret is the release's, in the release namespace; read it from the pod's own setting.
				dsn := strings.Split(podEnv(deployment(t, objs))["AGENTKOURIER_POSTGRES_DSN_SECRET"], "/")
				wantRefs = append(wantRefs, dsn[0]+"/"+dsn[1])
			}
			if len(wantRefs) == 0 {
				t.Fatal("the values name no Secret: the test proves nothing")
			}
			// namespace -> names; nil names is the whole namespace
			rolesIn := func(objs []object) map[string][]string {
				out := map[string][]string{}
				for _, o := range objs {
					if o.kind() == "Role" && strings.HasSuffix(o.name(), "-secrets") {
						var r rbacv1.Role
						find(t, []object{o}, "Role", o.name(), &r)
						if len(r.Rules) != 1 {
							t.Fatalf("Role %s/%s has %d rules", r.Namespace, r.Name, len(r.Rules))
						}
						out[r.Namespace] = r.Rules[0].ResourceNames
					}
				}
				return out
			}
			granted := rolesIn(objs)
			for _, ref := range wantRefs {
				ns, name, _ := strings.Cut(ref, "/")
				names, has := granted[ns]
				if !has || !slices.Contains(names, name) {
					t.Errorf("no Role in %s names Secret %s (Roles: %v)", ns, name, granted)
				}
			}
			filesRoles := rolesIn(files)
			for ns, names := range filesRoles {
				if len(names) != 0 {
					t.Errorf("the files route's Role in %s names %v: it is namespace-wide there", ns, names)
				}
				if _, has := granted[ns]; !has {
					t.Errorf("the files route grants namespace %s, and crds does not", ns)
				}
			}
			for ns := range granted {
				if _, has := filesRoles[ns]; !has {
					t.Errorf("crds grants namespace %s, which the files route does not", ns)
				}
			}
		})
	}
}

// TestCRModeRefusals: what a values file must not get past with config.source crds, and what it must not set with files.
func TestCRModeRefusals(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=registry.example.test/agent-kourier"
	twoConnections := `config.chatConnections={"a":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"a"}}},"b":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"b"}}}}`
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"the CRDs are not served", []string{"-f", values("crds")}, "serve them"},
		{"the CRDs are not served and crds.install is set", []string{"-f", values("crds"), "--set", "crds.install=true"}, "crds.install installs them with this release, too late for it"},
		{"Kubernetes 1.29", []string{"-f", values("crds"), "--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.29.9"}, "needs Kubernetes 1.30 or later"},
		{"a vendor build of 1.29", []string{"-f", values("crds"), "--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.29.14-gke.1"}, "needs Kubernetes 1.30 or later"},
		{"an existing ConfigMap", crMode("--set", image+",config.source=crds,config.chatConnection=c,config.existingConfigMap=mine"), "config.existingConfigMap is for config.source files"},
		{"no connection named, and none given", crMode("--set", image+",config.source=crds"), "needs config.chatConnection"},
		{"two connections and none chosen", crMode("--set", image+",config.source=crds", "--set-json", twoConnections), "needs config.chatConnection"},
		{"a connection with files", []string{"--set", image + ",config.chatConnection=c"}, "config.chatConnection is for config.source crds"},
		{"Secret names with files", []string{"--set-json", `rbac.secretNames={"ns":["a"]}`, "--set", image}, "rbac.secretNames is for config.source crds"},
		{"a connection of three parts", crMode("--set", image+",config.source=crds,config.chatConnection=a/b/c"), "config.chatConnection"},
		{"a connection in capitals", crMode("--set", image+",config.source=crds,config.chatConnection=Slack"), "config.chatConnection"},
		{"an unknown source", []string{"--set", image + ",config.source=crd"}, "config.source"},
		{"a Secret name with a slash", crMode("--set", image+",config.source=crds,config.chatConnection=c", "--set-json", `rbac.secretNames={"ns":["a/b"]}`), "rbac.secretNames"},
		{"a typo in crds.install", []string{"--set", image + ",crds.instal=true"}, "specifications of the schema"},
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

// TestCRModeKubeVersions: the minimum is 1.30 and a vendor suffix does not turn a good version away; helm's own default for a render that
// names no version (this helm's, which is newer) is above it.
func TestCRModeKubeVersions(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, v := range []string{"v1.30.0", "v1.30.0-eks-abc", "v1.30.2-gke.1", "v1.31.4", "v1.37.0"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if _, err := render(t.Context(), t, "agent-kourier", "agent-kourier", "-f", values("crds"), "--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", v); err != nil {
				t.Errorf("Kubernetes %s: %v; want it to render", v, err)
			}
		})
	}
}

// TestChatConnectionForms: the connection the broker serves is named as namespace/name, whichever way the values say it.
func TestChatConnectionForms(t *testing.T) {
	t.Parallel()
	helmPath(t)
	one := func(ns string) string {
		ns2 := ""
		if ns != "" {
			ns2 = `"namespace":"` + ns + `",`
		}
		return `config.chatConnections={"slack":{` + ns2 + `"spec":{"platform":"slack","credentialsSecretRef":{"name":"s"}}}}`
	}
	tests := []struct {
		name, set, setJSON, want string
	}{
		{"a name, in the release namespace", "config.chatConnection=slack", "", "rel-ns/slack"},
		{"namespace and name", "config.chatConnection=platform/slack", "", "platform/slack"},
		{"the one entry, in the release namespace", "", one(""), "rel-ns/slack"},
		{"the one entry, in its own namespace", "", one("platform"), "platform/slack"},
		{"a name beside an entry", "config.chatConnection=other", one("platform"), "rel-ns/other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := crMode("--set", "image.repository=agent-kourier,config.source=crds")
			if tc.set != "" {
				args = append(args, "--set", tc.set)
			}
			if tc.setJSON != "" {
				args = append(args, "--set-json", tc.setJSON)
			}
			env := podEnv(deployment(t, mustRender(t, "agent-kourier", "rel-ns", args...)))
			if env["AGENTKOURIER_CHAT_CONNECTION"] != tc.want {
				t.Errorf("AGENTKOURIER_CHAT_CONNECTION %q; want %q", env["AGENTKOURIER_CHAT_CONNECTION"], tc.want)
			}
		})
	}
}

// TestCRModeRBACFollowsTheValues: rbac.create=false makes no Role, ClusterRole or binding with crds either, the ClusterRole's name carries the
// release namespace (two releases of one name in two namespaces share a cluster), and what the ClusterRole names is what the chart's CRDs are.
func TestCRModeRBACFollowsTheValues(t *testing.T) {
	t.Parallel()
	helmPath(t)
	args := func(ns string, more ...string) []object {
		return mustRender(t, "agent-kourier", ns, append(append([]string{"-f", values("crds")}, crdsServed...), more...)...)
	}
	for _, o := range args("agent-kourier", "--set", "rbac.create=false") {
		if strings.HasPrefix(o.kind(), "Role") || strings.HasPrefix(o.kind(), "ClusterRole") {
			t.Errorf("rbac.create=false still rendered %s %s", o.kind(), o.name())
		}
	}
	names := map[string]bool{}
	for _, ns := range []string{"one", "two"} {
		for _, o := range args(ns) {
			if o.kind() == "ClusterRole" {
				names[o.name()] = true
			}
		}
	}
	if len(names) != 2 {
		t.Errorf("ClusterRole names %v for one release name in two namespaces; want two, since a ClusterRole has no namespace", names)
	}

	objs := args("agent-kourier", "--set", "crds.install=true")
	var cr rbacv1.ClusterRole
	find(t, objs, "ClusterRole", "agent-kourier-agent-kourier-crs", &cr)
	var crdNames, plurals []string
	for _, o := range objs {
		if o.kind() == "CustomResourceDefinition" {
			crdNames = append(crdNames, o.name())
			plurals = append(plurals, str(sub(sub(o, "spec"), "names")["plural"]))
		}
	}
	slices.Sort(plurals)
	if got := cr.Rules[2].ResourceNames; !slices.Equal(got, crdNames) {
		t.Errorf("the ClusterRole gets CRDs %v; the chart's are %v", got, crdNames)
	}
	if got := cr.Rules[0].Resources; !slices.Equal(got, plurals) {
		t.Errorf("the ClusterRole reads %v; the CRDs' plurals are %v", got, plurals)
	}
}

// TestCRModeNeedsNoConfig: custom resources that teams make are the point of the route, so a release with none of its own renders: no
// resource of the three kinds, the connection by name, and no Secret Role (nothing is named), but the ClusterRole.
func TestCRModeNeedsNoConfig(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", crMode("--set", "image.repository=agent-kourier,config.source=crds,config.chatConnection=platform/slack")...)
	for _, o := range objs {
		if strings.HasPrefix(str(o["apiVersion"]), "agentkourier.dev/") || o.kind() == "ConfigMap" || o.kind() == "Role" || o.kind() == "RoleBinding" {
			t.Errorf("rendered %s %s from values that have no config", o.kind(), o.name())
		}
	}
	if !slices.Contains(kinds(objs), "ClusterRole//agent-kourier-agent-kourier-crs") {
		t.Errorf("objects %v; want the ClusterRole", kinds(objs))
	}
	if env := podEnv(deployment(t, objs)); env["AGENTKOURIER_CHAT_CONNECTION"] != "platform/slack" {
		t.Errorf("AGENTKOURIER_CHAT_CONNECTION %q; want platform/slack", env["AGENTKOURIER_CHAT_CONNECTION"])
	}
}
