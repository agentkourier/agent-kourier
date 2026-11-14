package chart_test

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// The chart's two config routes (spec D50): crds, the default, which reads Binding, AgentBackend and ChatConnection custom resources, and files,
// which mounts a ConfigMap. These tests hold the default to what the broker needs of it, and the files route to what it always rendered.

// crdsServed makes helm template render as if the cluster already served the CRDs on a Kubernetes that CR mode accepts: the render of
// config.source crds refuses to go on otherwise (a CRD and the resources of it cannot be in one release's first apply).
var crdsServed = []string{"--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.33.2"}

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

// TestDefaultsAreTheCRRoute: with config.source and crds.install at their defaults, the chart is on the route of custom resources: the
// three CRDs, the ClusterRole that reads them, the pod told which ChatConnection it serves and mounting no ConfigMap, and no custom resource
// or ConfigMap, since the config.* values are empty and the teams apply their own. The CRDs are not served when helm template renders,
// and the render goes on: nothing in it is a resource of those kinds, so nothing can fail where Helm maps one.
func TestDefaultsAreTheCRRoute(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "--kube-version", "v1.33.2", "--set", "image.repository=agent-kourier,config.chatConnection=platform/slack")
	var crds []string
	for _, o := range objs {
		switch {
		case o.kind() == "CustomResourceDefinition":
			crds = append(crds, o.name())
		case o.kind() == "ConfigMap", strings.HasPrefix(str(o["apiVersion"]), "agentkourier.dev/"):
			t.Errorf("the defaults rendered %s %s: the config.* values are empty, so no resource and no ConfigMap is the chart's to make", o.kind(), o.name())
		}
	}
	if want := []string{"agentbackends.agentkourier.dev", "bindings.agentkourier.dev", "chatconnections.agentkourier.dev"}; !slices.Equal(crds, want) {
		t.Errorf("CRDs %v; want %v: crds.install follows the route, which is crds", crds, want)
	}
	if !slices.Contains(kinds(objs), "ClusterRole//agent-kourier-agent-kourier-crs") {
		t.Errorf("objects %v; want the ClusterRole that reads the custom resources", kinds(objs))
	}
	d := deployment(t, objs)
	env := podEnv(d)
	if env["AGENTKOURIER_CONFIG_SOURCE"] != "crds" || env["AGENTKOURIER_CHAT_CONNECTION"] != "platform/slack" {
		t.Errorf("AGENTKOURIER_CONFIG_SOURCE %q, AGENTKOURIER_CHAT_CONNECTION %q; want crds and platform/slack", env["AGENTKOURIER_CONFIG_SOURCE"], env["AGENTKOURIER_CHAT_CONNECTION"])
	}
	if _, has := env["AGENTKOURIER_CONFIG_DIR"]; has {
		t.Errorf("the pod sets AGENTKOURIER_CONFIG_DIR with the default route: there is no directory to read")
	}
	if slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.ConfigMap != nil }) {
		t.Errorf("volumes %+v; want no ConfigMap mounted with custom resources", d.Spec.Template.Spec.Volumes)
	}
}

// TestAnUpgradeThatKeepsTheOldValuesIsRefusedWithAWayOut: an install of the ConfigMap route that upgrades with the values it had, and so gets the
// default route, must not be moved silently: where the chart can tell, it refuses, and the message names what keeps the install working
// (config.source: files) and where the move is described. What no render can tell is the last case: the inline config, with the CRDs already
// served, is the move itself.
func TestAnUpgradeThatKeepsTheOldValuesIsRefusedWithAWayOut(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=registry.example.test/agent-kourier"
	inline := `config.chatConnections={"slack":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"s"}}}}`
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"a ConfigMap of its own", []string{"--set", image + ",config.existingConfigMap=mine"},
			[]string{"config.existingConfigMap is for config.source files", "set config.source: files", "how-to/migrate-config-source/"}},
		{"config in the values, on a cluster that does not serve the CRDs", []string{"--set", image, "--set-json", inline},
			[]string{"config.source: files", "how-to/migrate-config-source/"}},
		{"config in the values, and the CRDs are being installed with it", []string{"--set", image + ",crds.install=true", "--set-json", inline},
			[]string{"crds.install installs them with this release, too late for the resources of it", "config.source: files"}},
		{"Kubernetes 1.32", []string{"--set", image + ",config.chatConnection=c", "--kube-version", "v1.32.9"},
			[]string{"config.source crds needs Kubernetes 1.33 or later (it is the chart's default unless your values set it): the API server is v1.32.9", "set config.source: files"}},
		{"Kubernetes 1.32 and a ConfigMap of its own", []string{"--set", image + ",config.existingConfigMap=mine", "--kube-version", "v1.32.9"},
			[]string{"config.existingConfigMap is for config.source files"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", tc.args...)
			if err == nil {
				t.Fatalf("the chart rendered; want a refusal containing %q", tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("render error = %v; want one containing %q", err, want)
				}
			}
		})
	}

	// With the CRDs served the default route renders the inline config as custom resources, and the ConfigMap goes: this is the second step of the
	// move on the migration page, and nothing in a render tells it from a first install of the route. A cluster whose CRDs are served has
	// been prepared for it, and the page's dry run is how to check the objects before the upgrade.
	objs := mustRender(t, "agent-kourier", "agent-kourier", append(crMode("--set", image), "--set-json", inline)...)
	if !slices.Contains(kinds(objs), "ChatConnection/agent-kourier/slack") || slices.ContainsFunc(objs, func(o object) bool { return o.kind() == "ConfigMap" }) {
		t.Errorf("objects %v; want the ChatConnection as a custom resource and no ConfigMap", kinds(objs))
	}
}

// TestDocumentedOwnAppInstallsGrantTheConnectionsSecret: the install the chart README and Install with Helm give for a broker with a Slack app of its
// own names the ChatConnection and applies the resources themselves, so the chart sees no Secret reference. Their values must still render a Role for
// the connection's Secret (named in rbac.secretNames, or derived from the connection's name), or the broker, which cannot read its own ChatConnection's
// Secret, exits with code 3 in a loop.
func TestDocumentedOwnAppInstallsGrantTheConnectionsSecret(t *testing.T) {
	t.Parallel()
	helmPath(t)
	install, err := os.ReadFile("../../docs/how-to/install-with-helm.md")
	if err != nil {
		t.Fatal(err)
	}
	// The Secret the documented own ChatConnection, the one in the release's namespace, reads, from the resources the page applies.
	m := regexp.MustCompile("kind: ChatConnection\nmetadata:\n  name: slack\n  namespace: agent-kourier\nspec:\n  platform: slack\n  credentialsSecretRef: \\{name: ([a-z0-9-]+)\\}").FindSubmatch(install)
	if m == nil {
		t.Fatal("Install with Helm has no ChatConnection slack in agent-kourier with a credentialsSecretRef")
	}
	// The chart README carries no resources of its own: its snippet relies on the chart naming the Secret after the connection, which is the Secret
	// that Install with Helm's ChatConnection reads, so both documents are held to the same one.
	secret := string(m[1])
	for _, doc := range []string{"../../docs/how-to/install-with-helm.md", chartReadme} {
		t.Run(filepath.Base(doc), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(doc)
			if err != nil {
				t.Fatal(err)
			}
			var block string
			for _, part := range strings.Split(string(raw), "```yaml")[1:] {
				body, _, _ := strings.Cut(part, "\n```")
				if strings.Contains(body, "chatConnection: agent-kourier/slack") {
					_, block, _ = strings.Cut(body, "\n")
					break
				}
			}
			if block == "" {
				t.Fatalf("%s has no values block that names config.chatConnection agent-kourier/slack", doc)
			}
			file := filepath.Join(t.TempDir(), "my-values.yaml")
			if err := os.WriteFile(file, []byte(block), 0o600); err != nil {
				t.Fatal(err)
			}
			objs := mustRender(t, "agent-kourier", "agent-kourier", "--kube-version", "v1.33.2", "--set", "image.repository=agent-kourier", "-f", file)
			var role rbacv1.Role
			find(t, objs, "Role", "agent-kourier-secrets", &role)
			if role.Namespace != "agent-kourier" || len(role.Rules) != 1 || !slices.Contains(role.Rules[0].ResourceNames, secret) {
				t.Errorf("Role %+v in %q; want one rule that names the Secret %s in agent-kourier", role.Rules, role.Namespace, secret)
			}
		})
	}
}

// TestDocumentedTeamAppInstalls: the install that the chart README and Install with Helm lead with is a broker with no Slack app of its own. The
// values of both are comments only (Install with Helm's is what init writes), which show team-a in config.connectionNamespaces commented out
// and render every namespace: no list, no selector and no own connection. Both must grant no Secret, since both say the team grants its own,
// and the ServiceAccount their RoleBinding names must be the one the chart renders.
func TestDocumentedTeamAppInstalls(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, doc := range []string{"../../docs/how-to/install-with-helm.md", chartReadme} {
		t.Run(filepath.Base(doc), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(doc)
			if err != nil {
				t.Fatal(err)
			}
			var block string
			for _, part := range strings.Split(string(raw), "```yaml")[1:] {
				body, _, _ := strings.Cut(part, "\n```")
				if strings.Contains(body, "connectionNamespaces: [team-a]") {
					_, block, _ = strings.Cut(body, "\n")
					break
				}
			}
			if block == "" {
				t.Fatalf("%s has no values block that shows team-a in config.connectionNamespaces", doc)
			}
			file := filepath.Join(t.TempDir(), "my-values.yaml")
			if err := os.WriteFile(file, []byte(block), 0o600); err != nil {
				t.Fatal(err)
			}
			objs := mustRender(t, "agent-kourier", "agent-kourier", "--kube-version", "v1.33.2", "--set", "image.repository=agent-kourier", "-f", file)
			env := podEnv(deployment(t, objs))
			if v, has := env["AGENTKOURIER_CHAT_CONNECTION"]; has || env["AGENTKOURIER_CONFIG_SOURCE"] != "crds" {
				t.Errorf("environment %v (own connection %q); want crds and no own connection", env, v)
			}
			if list, has := env["AGENTKOURIER_CONNECTION_NAMESPACES"]; has {
				t.Errorf("the list is %q; want none (every namespace)", list)
			}
			if sel, has := env["AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"]; has {
				t.Errorf("the selector is %q; want none (every namespace)", sel)
			}
			if got := secretRoles(t, objs); len(got) != 0 {
				t.Errorf("Secret Roles %v; want none (the team grants its own)", got)
			}
			ms := regexp.MustCompile(`--serviceaccount=([a-z0-9-]+):([a-z0-9-]+)`).FindAllStringSubmatch(string(raw), -1)
			if ms == nil {
				t.Fatalf("%s has no RoleBinding command that names the broker's ServiceAccount", doc)
			}
			for _, m := range ms {
				if !slices.ContainsFunc(objs, func(o object) bool {
					return o.kind() == "ServiceAccount" && o.namespace() == m[1] && o.name() == m[2]
				}) {
					t.Errorf("%s grants the ServiceAccount %s:%s, which the chart does not render; objects %v", doc, m[1], m[2], kinds(objs))
				}
			}
		})
	}
}

// TestCRDsInstallFollowsTheRoute: an unset crds.install is true with config.source crds and false with files, and an explicit value wins. The
// four cases that matter: files leaves the CRDs out; crds installs them; files with true installs them, which is step 1 of the move; and crds
// with false installs none and so needs them served, which the chart checks.
func TestCRDsInstallFollowsTheRoute(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=registry.example.test/agent-kourier"
	crdNames := func(objs []object) (names []string) {
		for _, o := range objs {
			if o.kind() == "CustomResourceDefinition" {
				names = append(names, o.name())
			}
		}
		return names
	}
	three := []string{"agentbackends.agentkourier.dev", "bindings.agentkourier.dev", "chatconnections.agentkourier.dev"}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"files, crds.install unset", []string{"--set", image + ",config.source=files,config.existingConfigMap=mine"}, nil},
		{"crds, crds.install unset", []string{"--set", image + ",config.chatConnection=platform/slack"}, three},
		{"files, crds.install true: step 1 of the move", []string{"--set", image + ",config.source=files,crds.install=true,config.existingConfigMap=mine"}, three},
		{"crds, crds.install false, the CRDs served", crMode("--set", image+",crds.install=false,config.chatConnection=platform/slack"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := crdNames(mustRender(t, "agent-kourier", "agent-kourier", tc.args...)); !slices.Equal(got, tc.want) {
				t.Errorf("CRDs %v; want %v", got, tc.want)
			}
		})
	}
	// With false on the route of custom resources the release installs no CRD, so they must be served already: the check that an unset value
	// skips for a release with no config.* runs. It names the way out.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"nothing in config.*", []string{"--set", image + ",crds.install=false,config.chatConnection=platform/slack"}},
		{"a resource in config.*", []string{"--set", image + ",crds.install=false", "-f", values("crds")}},
	} {
		t.Run("crds, crds.install false, not served, "+tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", tc.args...)
			if err == nil || !strings.Contains(err.Error(), "needs the CRDs (group agentkourier.dev, version v1alpha1) to be installed before this release") {
				t.Errorf("render error = %v; want the refusal that the CRDs are not served", err)
			}
		})
	}
}

// TestTierValuesNameTheirRoute: the default route moved, so every values file of the repository that means the ConfigMap route says so, and
// does not ask for the CRDs, and the one that means custom resources says so too. A values file that leans on the default would change which
// route a kind tier or a golden file runs.
func TestTierValuesNameTheirRoute(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(chartDir, "ci", "*-values.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no ci values files: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), "-values.yaml")
		if name == "fast-postgres-ha" { // an overlay on fast-postgres: it sets neither
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var v struct {
				Config struct {
					Source string `json:"source"`
				} `json:"config"`
				CRDs struct {
					Install *bool `json:"install"`
				} `json:"crds"`
			}
			if err := yaml.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			want := "files"
			if name == "crds" {
				want = "crds"
			}
			if v.Config.Source != want {
				t.Errorf("config.source = %q; want %q, named in the file", v.Config.Source, want)
			}
			// An unset crds.install follows the route, so a file on files may leave it out; it must never say true, which would add the CRDs to what the
			// file renders. The file of custom resources says false: its platform applies the CRDs itself.
			if want == "files" && v.CRDs.Install != nil && *v.CRDs.Install {
				t.Errorf("crds.install = true; want it unset or false: these files render the ConfigMap route and no CRD")
			}
			if want == "crds" && (v.CRDs.Install == nil || *v.CRDs.Install) {
				t.Errorf("crds.install = %v; want false, named in the file: the platform installs the CRDs itself", v.CRDs.Install)
			}
		})
	}
}

// TestExplicitFilesRouteIsTheConfigMapRoute: with config.source files, which every values file that means it now says, the chart renders what it
// rendered before the route of custom resources was the default (the golden files say so object by object); this says what that means. No
// CRD, ClusterRole or custom resource, no environment variable of the other route, and the ConfigMap mounted where the broker reads its directory.
func TestExplicitFilesRouteIsTheConfigMapRoute(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range goldenCases {
		if tc.name == "crds" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, tc.release, tc.namespace, "-f", values(tc.name))
			for _, o := range objs {
				if o.kind() == "CustomResourceDefinition" || o.kind() == "ClusterRole" || o.kind() == "ClusterRoleBinding" || strings.HasPrefix(str(o["apiVersion"]), "agentkourier.dev/") {
					t.Errorf("the files route rendered %s %s, which only the route of custom resources makes", o.kind(), o.name())
				}
			}
			d := deployment(t, objs)
			env := podEnv(d)
			for _, name := range []string{"AGENTKOURIER_CONFIG_SOURCE", "AGENTKOURIER_CHAT_CONNECTION"} {
				if _, has := env[name]; has {
					t.Errorf("the pod sets %s on the files route: its template would move for every install", name)
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
// spec of each is the file's, byte for byte (the file is not parsed and written again), with the chart's labels, resource-policy keep and Argo CD's Prune=false,Delete=false added to the
// metadata. The hash annotation is the file's own (the SHA-256 of the file without that line), passed on as it is.
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
			if ann["argocd.argoproj.io/sync-options"] != "Prune=false,Delete=false" {
				t.Errorf("annotations %v; want argocd.argoproj.io/sync-options Prune=false,Delete=false, so that Argo CD does not prune the CRD or delete it with the Application", ann)
			}
			// The hash is the file's own annotation, which the chart passes on as it is: it is the SHA-256 of the file without that line.
			bare := schemaHashLine.ReplaceAll(raw, nil)
			if ann[v1alpha1.SchemaHashAnnotation] != sha256Hex(bare) || sub(want, "metadata")["annotations"].(map[string]any)[v1alpha1.SchemaHashAnnotation] != sha256Hex(bare) {
				t.Errorf("schema-sha256 %v; want the SHA-256 of %s without its annotation line, %s", ann[v1alpha1.SchemaHashAnnotation], f, sha256Hex(bare))
			}
			// The hash the broker is built with is the one the rendered CRD carries: the broker compares them.
			if built, ok := v1alpha1.SchemaSHA256(want.name()); !ok || ann[v1alpha1.SchemaHashAnnotation] != built {
				t.Errorf("the rendered %s carries %v; the broker is built with %q (%v)", want.name(), ann[v1alpha1.SchemaHashAnnotation], built, ok)
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

// schemaHashLine is the one line of a generated CRD that its hash does not cover.
var schemaHashLine = regexp.MustCompile(`(?m)^    agentkourier\.dev/schema-sha256: [0-9a-f]{64}\n`)

// sha256Hex is the hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// decodeCR passes a rendered custom resource through the decoder of the file loader, which the broker's CR source uses too. The broker
// tolerates a field it does not know and the chart must not render one, so each is an error here.
func decodeCR(t *testing.T, kind string, o object) []error {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var unknown []string
	var errs []error
	switch kind {
	case "AgentBackend":
		_, unknown, errs = config.DecodeObject[v1alpha1.AgentBackend](b)
	case "ChatConnection":
		_, unknown, errs = config.DecodeObject[v1alpha1.ChatConnection](b)
	case "Binding":
		_, unknown, errs = config.DecodeObject[v1alpha1.Binding](b)
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	for _, f := range unknown {
		errs = append(errs, fmt.Errorf("%s: unknown field", f))
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

// secretRoles returns, by namespace, the resourceNames of each Secret Role of a render (an empty list is a Role for the whole namespace).
func secretRoles(t *testing.T, objs []object) map[string][]string {
	t.Helper()
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

// TestCRModeGrantsTheServedConnectionsSecret: the chart cannot see the ChatConnection a team applies, but it knows which one the broker serves, and a
// broker that cannot read that connection's Secret exits at start. So config.chatConnection alone derives a Role that names one Secret: the
// connection's name, or config.chatConnectionSecret, in the connection's namespace (a reference is namespace-local). It is de-duplicated against
// rbac.secretNames and skipped when config.chatConnections renders the connection, whose own credentialsSecretRef the chart already names.
func TestCRModeGrantsTheServedConnectionsSecret(t *testing.T) {
	t.Parallel()
	helmPath(t)
	oneEntry := func(ns, secret string) string {
		nsField := ""
		if ns != "" {
			nsField = `"namespace":"` + ns + `",`
		}
		return `config.chatConnections={"slack":{` + nsField + `"spec":{"platform":"slack","credentialsSecretRef":{"name":"` + secret + `"}}}}`
	}
	tests := []struct {
		name string
		set  string
		json []string
		want map[string][]string
	}{
		{"a name alone: the Secret of that name in the release namespace", "config.chatConnection=slack", nil,
			map[string][]string{"rel-ns": {"slack"}}},
		{"namespace and name: the Role is in the connection's namespace", "config.chatConnection=platform/slack", nil,
			map[string][]string{"platform": {"slack"}}},
		{"chatConnectionSecret names another Secret", "config.chatConnection=slack,config.chatConnectionSecret=slack-bot", nil,
			map[string][]string{"rel-ns": {"slack-bot"}}},
		{"chatConnectionSecret in the connection's namespace", "config.chatConnection=platform/slack,config.chatConnectionSecret=slack-bot", nil,
			map[string][]string{"platform": {"slack-bot"}}},
		{"an empty chatConnectionSecret is the default", "config.chatConnection=slack,config.chatConnectionSecret=", nil,
			map[string][]string{"rel-ns": {"slack"}}},
		{"rbac.secretNames adds to it", "config.chatConnection=slack",
			[]string{`rbac.secretNames={"rel-ns":["agent-token"],"team-a":["team-token"]}`},
			map[string][]string{"rel-ns": {"agent-token", "slack"}, "team-a": {"team-token"}}},
		{"a name that rbac.secretNames repeats is named once", "config.chatConnection=slack",
			[]string{`rbac.secretNames={"rel-ns":["slack","agent-token"]}`},
			map[string][]string{"rel-ns": {"agent-token", "slack"}}},
		{"the same name in another namespace is another grant", "config.chatConnection=platform/slack",
			[]string{`rbac.secretNames={"rel-ns":["slack"]}`},
			map[string][]string{"platform": {"slack"}, "rel-ns": {"slack"}}},
		{"rbac.secretNamespaces still grants the namespace whole", "config.chatConnection=slack,rbac.secretNamespaces={rel-ns}", nil,
			map[string][]string{"rel-ns": nil}},
		{"a rendered connection of that name is granted by its own reference, not the derived one", "config.chatConnection=slack",
			[]string{oneEntry("", "slack-main")},
			map[string][]string{"rel-ns": {"slack-main"}}},
		{"a rendered connection in the connection's namespace", "config.chatConnection=platform/slack",
			[]string{oneEntry("platform", "slack-main")},
			map[string][]string{"platform": {"slack-main"}}},
		{"chatConnectionSecret does not rename a rendered connection's Secret", "config.chatConnection=slack,config.chatConnectionSecret=slack-bot",
			[]string{oneEntry("", "slack-main")},
			map[string][]string{"rel-ns": {"slack-main"}}},
		{"no chatConnection, one rendered connection: its own reference only", "", []string{oneEntry("", "slack-main")},
			map[string][]string{"rel-ns": {"slack-main"}}},
		{"a rendered connection of another namespace is not the served one", "config.chatConnection=slack",
			[]string{oneEntry("platform", "slack-main")},
			map[string][]string{"platform": {"slack-main"}, "rel-ns": {"slack"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := crMode("--set", "image.repository=agent-kourier,config.source=crds")
			if tc.set != "" {
				args = append(args, "--set", tc.set)
			}
			for _, j := range tc.json {
				args = append(args, "--set-json", j)
			}
			got := secretRoles(t, mustRender(t, "agent-kourier", "rel-ns", args...))
			if len(got) != len(tc.want) {
				t.Errorf("Secret Roles %v; want %v", got, tc.want)
			}
			for ns, names := range tc.want {
				if g, has := got[ns]; !has || !slices.Equal(g, names) {
					t.Errorf("Secret Role in %s names %v (present %v); want %v (Roles %v)", ns, g, has, names, got)
				}
			}
		})
	}
}

// TestKindOverlayGrantsTheTiersSecretsByName: the kind tiers' overlay for the custom-resource route lists only the Binding's token in rbac.secretNames; the
// connection's Secret, slack-fast, is the chart's to derive from config.chatConnection. Rendered over the fast tier's values, the namespace's Role names
// both and is not namespace-wide.
func TestKindOverlayGrantsTheTiersSecretsByName(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", crMode("-f", values("fast"), "-f", "../../hack/kind/crds-values.yaml")...)
	got := secretRoles(t, objs)
	if want := []string{"agent-kourier-agent-token", "slack-fast"}; !slices.Equal(got["agent-kourier"], want) {
		t.Errorf("Secret Roles %v; want agent-kourier to name %v", got, want)
	}
}

// TestCRModeConnectionSecretNameIsChecked: config.chatConnectionSecret is a Secret name, so the schema holds it to a DNS-1123 subdomain, as it does
// the names in rbac.secretNames, and the empty string (the default) is allowed.
func TestCRModeConnectionSecretNameIsChecked(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=agent-kourier,config.source=crds,config.chatConnection=slack"
	for _, bad := range []string{"Slack", "a/b", "-a", "a-", "a_b", strings.Repeat("a", 254)} {
		t.Run("refuses "+bad[:min(len(bad), 12)], func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", crMode("--set", image+",config.chatConnectionSecret="+bad)...)
			if err == nil || !strings.Contains(err.Error(), "chatConnectionSecret") {
				t.Errorf("render error = %v; want a refusal of config.chatConnectionSecret", err)
			}
		})
	}
	for _, good := range []string{"", "slack", "slack.bot-1", strings.Repeat("a", 253)} {
		t.Run("allows "+good[:min(len(good), 12)], func(t *testing.T) {
			t.Parallel()
			if _, err := render(t.Context(), t, "agent-kourier", "agent-kourier", crMode("--set", image+",config.chatConnectionSecret="+good)...); err != nil {
				t.Errorf("config.chatConnectionSecret=%q: %v; want it to render", good, err)
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
		{"the CRDs are not served and crds.install is set", []string{"-f", values("crds"), "--set", "crds.install=true"}, "crds.install installs them with this release, too late for the resources of it"},
		{"Kubernetes 1.32", []string{"-f", values("crds"), "--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.32.9"}, "needs Kubernetes 1.33 or later"},
		{"Kubernetes 1.30", []string{"-f", values("crds"), "--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.30.2"}, "needs Kubernetes 1.33 or later"},
		{"a vendor build of 1.32", []string{"-f", values("crds"), "--api-versions", "agentkourier.dev/v1alpha1", "--kube-version", "v1.32.14-gke.1"}, "needs Kubernetes 1.33 or later"},
		{"an existing ConfigMap", crMode("--set", image+",config.source=crds,config.chatConnection=c,config.existingConfigMap=mine"), "config.existingConfigMap is for config.source files"},
		{"two connections and none chosen", crMode("--set", image+",config.source=crds", "--set-json", twoConnections), "needs config.chatConnection"},
		{"a connection of three parts", crMode("--set", image+",config.source=crds,config.chatConnection=a/b/c"), "config.chatConnection"},
		{"a connection in capitals", crMode("--set", image+",config.source=crds,config.chatConnection=Slack"), "config.chatConnection"},
		{"an unknown source", []string{"--set", image + ",config.source=crd"}, "config.source"},
		{"a Secret name with a slash", crMode("--set", image+",config.source=crds,config.chatConnection=c", "--set-json", `rbac.secretNames={"ns":["a/b"]}`), "rbac.secretNames"},
		{"a typo in crds.install", []string{"--set", image + ",crds.instal=true"}, "specifications of the schema"},
		// The refusal is one for both routes: with the CRs the broker lists and watches through the API server as well.
		{"a NetworkPolicy without the API server", crMode("--set", image+",config.source=crds,config.chatConnection=c,networkPolicy.enabled=true"), "networkPolicy.kubeApiServer.cidrs"},
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

// TestRefusalsDoNotCallTheRouteTheDefaultOfAValuesFileThatSetIt: the values the chart sees hold crds whether the file said so or the chart's
// own values did, and no template can tell the two apart (the default is filled in before it runs; with --reuse-values an unset value reads
// as files instead). So a refusal that names the default says it is the default unless the values set the route, which is true either way,
// and never "(the chart's default)" outright, which told an operator who had set config.source: crds on purpose that he had not.
func TestRefusalsDoNotCallTheRouteTheDefaultOfAValuesFileThatSetIt(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=registry.example.test/agent-kourier"
	const unless = "the chart's default unless your values set it"
	twoConnections := `config.chatConnections={"a":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"a"}}},"b":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"b"}}}}`
	for _, set := range []struct{ name, route string }{{"config.source unset", ""}, {"config.source crds set", ",config.source=crds"}} {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"two connections and none named", []string{"--set", image + set.route, "--set-json", twoConnections, "--api-versions", "agentkourier.dev/v1alpha1"}},
			{"a ConfigMap of its own", []string{"--set", image + set.route + ",config.chatConnection=c,config.existingConfigMap=mine"}},
			{"Kubernetes 1.32", []string{"--set", image + set.route + ",config.chatConnection=c", "--kube-version", "v1.32.9"}},
			{"the CRDs are not served", []string{"--set", image + set.route + ",config.chatConnection=c,crds.install=false", "--kube-version", "v1.34.0"}},
		} {
			t.Run(set.name+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", tc.args...)
				if err == nil {
					t.Fatal("the chart rendered; want a refusal")
				}
				if !strings.Contains(err.Error(), unless) {
					t.Errorf("render error = %v; want one that says the route is %q", err, unless)
				}
				if strings.Contains(err.Error(), "default)") {
					t.Errorf("render error = %v; want no parenthesis that ends in \"default)\"", err)
				}
			})
		}
	}
}

// TestDocsQuoteOnlyRefusalsTheChartPrints: a page that quotes how a refusal of the config route begins quotes what _helpers.tpl prints. A quote is a
// code span that starts one of the refusals, cut at its first " ..." (the pages elide the middle); it must be the start of a refusal, up to the %s of
// one that has an argument. A reworded refusal fails here and not in a reader's terminal, where the page's search would no longer find it.
func TestDocsQuoteOnlyRefusalsTheChartPrints(t *testing.T) {
	t.Parallel()
	tpl, err := os.ReadFile(filepath.Join(chartDir, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	var refusals []string
	for _, m := range regexp.MustCompile(`fail (?:\(printf )?"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(string(tpl), -1) {
		refusals = append(refusals, m[1])
	}
	if len(refusals) == 0 {
		t.Fatal("no refusal found in _helpers.tpl")
	}
	starts := []string{"config.source crds needs", "config.source crds (", "config.existingConfigMap is for"}
	quote := regexp.MustCompile("`([^`\n]+)`")
	quoted := 0
	files := []string{"../../README.md", chartReadme}
	if err := filepath.WalkDir("../../docs", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "brand" || d.Name() == "spikes") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range quote.FindAllStringSubmatch(string(raw), -1) {
			span := m[1]
			if !slices.ContainsFunc(starts, func(s string) bool { return strings.HasPrefix(span, s) }) {
				continue
			}
			quoted++
			head, _, _ := strings.Cut(span, " ...")
			head = strings.TrimRight(head, " ")
			if !slices.ContainsFunc(refusals, func(r string) bool {
				if before, _, ok := strings.Cut(r, "%s"); ok && strings.HasPrefix(head, before) {
					return true
				}
				return strings.HasPrefix(r, head)
			}) {
				t.Errorf("%s quotes %q, which no refusal in _helpers.tpl begins with", file, head)
			}
		}
	}
	if quoted < 6 {
		t.Errorf("%d quotes of a refusal found in the docs; want at least 6, so the search has not stopped finding them", quoted)
	}
}

// TestCRModeKubeVersions: the minimum is 1.33 and a vendor suffix does not turn a good version away; helm's own default for a render that
// names no version (this helm's, which is newer) is above it.
func TestCRModeKubeVersions(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, v := range []string{"v1.33.0", "v1.33.0-eks-abc", "v1.33.2-gke.1", "v1.34.1", "v1.37.0"} {
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

	// A namespace in both rbac.secretNamespaces and rbac.secretNames is granted whole: the names do not narrow what was asked for in full. The
	// values file names platform in secretNames only, so its Role has names; adding it to secretNamespaces drops them.
	platformRule := func(objs []object) rbacv1.PolicyRule {
		for _, o := range objs {
			if o.kind() == "Role" && o.namespace() == "platform" && o.name() == "agent-kourier-secrets" {
				var r rbacv1.Role
				find(t, []object{o}, "Role", o.name(), &r)
				return r.Rules[0]
			}
		}
		t.Fatalf("no Secret Role in namespace platform: %v", kinds(objs))
		return rbacv1.PolicyRule{}
	}
	if rule := platformRule(args("agent-kourier")); !slices.Equal(rule.ResourceNames, []string{"shared-token"}) {
		t.Errorf("platform's Role names %v; want shared-token (it is in secretNames only)", rule.ResourceNames)
	}
	if rule := platformRule(args("agent-kourier", "--set", "rbac.secretNamespaces={team-b,platform}")); len(rule.ResourceNames) != 0 {
		t.Errorf("platform's Role names %v although the namespace is in rbac.secretNamespaces; want the whole namespace", rule.ResourceNames)
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
// resource of the three kinds, the connection by name, the one Secret Role that the connection's name derives (and no other), and the ClusterRole.
func TestCRModeNeedsNoConfig(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", crMode("--set", "image.repository=agent-kourier,config.source=crds,config.chatConnection=platform/slack")...)
	for _, o := range objs {
		if strings.HasPrefix(str(o["apiVersion"]), "agentkourier.dev/") || o.kind() == "ConfigMap" {
			t.Errorf("rendered %s %s from values that have no config", o.kind(), o.name())
		}
		if (o.kind() == "Role" || o.kind() == "RoleBinding") && o.namespace() != "platform" {
			t.Errorf("rendered %s %s in %q; want only the connection's Secret Role in platform", o.kind(), o.name(), o.namespace())
		}
	}
	if !slices.Contains(kinds(objs), "ClusterRole//agent-kourier-agent-kourier-crs") {
		t.Errorf("objects %v; want the ClusterRole", kinds(objs))
	}
	if env := podEnv(deployment(t, objs)); env["AGENTKOURIER_CHAT_CONNECTION"] != "platform/slack" {
		t.Errorf("AGENTKOURIER_CHAT_CONNECTION %q; want platform/slack", env["AGENTKOURIER_CHAT_CONNECTION"])
	}
}

// TestFilesRouteHasNoMinimumVersion: the version check belongs to config.source crds alone. The ConfigMap route renders on an old API server,
// with or without crds.install, so no default path gained a check.
func TestFilesRouteHasNoMinimumVersion(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, extra := range [][]string{nil, {"--set", "crds.install=true"}} {
		args := append([]string{"-f", values("minimal"), "--kube-version", "v1.20.0"}, extra...)
		if _, err := render(t.Context(), t, "agent-kourier", "agent-kourier", args...); err != nil {
			t.Errorf("render on Kubernetes 1.20 with %v: %v; want it to render", extra, err)
		}
	}
}

// TestUpgradeFromAReleaseThatLacksTheNewValues: `helm upgrade --reuse-values` from release 0.1.6 hands the chart the values of that release, which
// have no crds, config.source, config.chatConnection or rbac.secretNames, and the chart must read each as unset: it renders, and renders what the
// defaults render. (A release made with those values at the defaults is the same render.) The first version of this case was run on kind, from the released chart, with Helm 4.3.0, while the default was still files (the commit that added the route's chart); with the default now crds, the case is simulated by these renders with the key set to null, and what Helm hands the chart under `--reuse-values` is not measured.
func TestUpgradeFromAReleaseThatLacksTheNewValues(t *testing.T) {
	t.Parallel()
	helmPath(t)
	base := []string{"-f", values("minimal")}
	want, err := render(t.Context(), t, "agent-kourier", "agent-kourier", base...)
	if err != nil {
		t.Fatal(err)
	}
	for _, unset := range []string{"crds", "config.source", "config.chatConnection", "rbac.secretNames", "crds.install", "config.connectionNamespaceSelector"} {
		t.Run(unset, func(t *testing.T) {
			t.Parallel()
			got, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), "--set", unset+"=null")...)
			if err != nil {
				t.Fatalf("with %s unset: %v", unset, err)
			}
			if got != want {
				t.Errorf("with %s unset the render differs from the defaults':\n%s", unset, firstDifference(want, got))
			}
		})
	}
	all, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), "--set", "crds=null,config.source=null,config.chatConnection=null,rbac.secretNames=null")...)
	if err != nil || all != want {
		t.Errorf("with all four unset: %v\n%s", err, firstDifference(want, all))
	}
}

// TestFilesRouteIgnoresTheCRModeValues: going back to files keeps the values of the route of custom resources, and the chart does not refuse them:
// they are ignored, and the render is the defaults'. An operator rolling back in an incident is not stopped by the chart.
func TestFilesRouteIgnoresTheCRModeValues(t *testing.T) {
	t.Parallel()
	helmPath(t)
	base := []string{"-f", values("minimal")}
	want, err := render(t.Context(), t, "agent-kourier", "agent-kourier", base...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), "--set", "config.source=files,config.chatConnection=agent-kourier/slack-main,config.chatConnectionSecret=slack-bot",
		"--set-json", `rbac.secretNames={"agent-kourier":["slack-main"]}`)...)
	if err != nil {
		t.Fatalf("files with the values of the CR route: %v", err)
	}
	if got != want {
		t.Errorf("the values of the CR route change the files route's render:\n%s", firstDifference(want, got))
	}
}

// TestTokenRefreshInCRModeDoesNotNeedTheBindingInTheValues: with config.source crds a team applies its own Binding, and the chart cannot see it in
// config.*, so the check that some Binding names the refreshed Secret (which the files route makes) does not apply: the render goes on, and
// the Job's Role is in the namespace it is told. The same values on the files route are still refused.
func TestTokenRefreshInCRModeDoesNotNeedTheBindingInTheValues(t *testing.T) {
	t.Parallel()
	helmPath(t)
	refresh := "tokenRefresh.enabled=true,tokenRefresh.targetSecret=team-token,tokenRefresh.targetNamespace=team-a,tokenRefresh.dex.tokenURL=http://dex,tokenRefresh.dex.clientId=c,tokenRefresh.dex.credentialsSecret=d"
	objs := mustRender(t, "agent-kourier", "agent-kourier", append([]string{"-f", values("crds"), "--set", refresh}, crdsServed...)...)
	var r rbacv1.Role
	find(t, objs, "Role", "agent-kourier-token-refresh", &r)
	if r.Namespace != "team-a" || len(r.Rules) != 1 || !slices.Equal(r.Rules[0].ResourceNames, []string{"team-token"}) {
		t.Errorf("Role %+v in %q; want the one rule for team-token, in team-a, where the hand-made Binding is", r.Rules, r.Namespace)
	}
	_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", "-f", values("crds"), "--set", "config.source=files,"+refresh)
	if err == nil || !strings.Contains(err.Error(), "no Binding in namespace team-a has identity.tokenSecretRef naming it") {
		t.Errorf("the files route with the same values: %v; want the refusal that no Binding reads the Secret", err)
	}
}

// TestConnectionNamespaces: config.connectionNamespaces is the platform's admission list. With crds the pod gets it as
// AGENTKOURIER_CONNECTION_NAMESPACES, comma-joined, right after the variable that names its own connection. Empty (the default, and an explicit empty
// list) it renders nothing and the render is what it was before the value existed. A list changes that one line and nothing else: the ClusterRole is the
// same, and the chart grants no Secret of a listed namespace, since each team grants its own.
func TestConnectionNamespaces(t *testing.T) {
	t.Parallel()
	helmPath(t)
	const key = "AGENTKOURIER_CONNECTION_NAMESPACES"
	// The broker keeps its own connection here (config.chatConnection set): with it empty, a list means no own connection, which
	// TestNoOwnConnection holds to its own rules.
	base := crMode("-f", values("crds"), "--set", "config.chatConnection=agent-kourier/slack-main")
	renderWith := func(more ...string) string {
		t.Helper()
		out, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), more...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	unset := renderWith()
	if strings.Contains(unset, key) {
		t.Errorf("%s is in the render with the list unset; want it absent", key)
	}
	if empty := renderWith("--set-json", "config.connectionNamespaces=[]"); empty != unset {
		t.Errorf("an empty list changes the render:\n%s", firstDifference(unset, empty))
	}

	tests := []struct {
		name string
		json string
		want string
	}{
		{"one namespace", `["team-a"]`, "team-a"},
		{"several, in the order given", `["team-b","team-a","platform"]`, "team-b,team-a,platform"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			listed := renderWith("--set-json", "config.connectionNamespaces="+tc.json)
			d := deployment(t, decode(t, listed))
			if got, has := podEnv(d)[key]; !has || got != tc.want {
				t.Errorf("%s = %q (set: %v); want %q", key, got, has, tc.want)
			}
			var names []string
			for _, e := range d.Spec.Template.Spec.Containers[0].Env {
				names = append(names, e.Name)
			}
			if i := slices.Index(names, "AGENTKOURIER_CHAT_CONNECTION"); i < 0 || i+1 >= len(names) || names[i+1] != key {
				t.Errorf("environment %v; want %s right after AGENTKOURIER_CHAT_CONNECTION", names, key)
			}
			var rest []string
			for _, line := range strings.Split(listed, "\n") {
				if !strings.Contains(line, key) {
					rest = append(rest, line)
				}
			}
			if got := strings.Join(rest, "\n"); got != unset {
				t.Errorf("the list changes more than its variable (the ClusterRole and the Secret Roles are the unset render's):\n%s", firstDifference(unset, got))
			}
		})
	}
}

// TestConnectionNamespacesAreRefusedWithFiles: the file route serves the one ChatConnection of its directory and a pod on it would silently
// stop serving the ChatConnections of the listed namespaces, so the chart refuses the list there, naming the route. An unset config.source reads as files
// (a release upgraded with --reuse-values), so it is refused the same. An empty list is no setting and renders.
func TestConnectionNamespacesAreRefusedWithFiles(t *testing.T) {
	t.Parallel()
	helmPath(t)
	list := []string{"--set-json", `config.connectionNamespaces=["team-a"]`}
	tests := []struct {
		name string
		args []string
	}{
		{"files", append([]string{"-f", values("minimal"), "--set", "config.source=files"}, list...)},
		{"config.source nulled, which reads as files", append([]string{"-f", values("minimal"), "--set", "config.source=null"}, list...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", tc.args...)
			if err == nil || !strings.Contains(err.Error(), "config.connectionNamespaces is for config.source crds") || !strings.Contains(err.Error(), "config.source: files") {
				t.Errorf("render error = %v; want the refusal that names config.connectionNamespaces and config.source: files", err)
			}
		})
	}
	if _, err := render(t.Context(), t, "agent-kourier", "agent-kourier", "-f", values("minimal"), "--set", "config.source=files", "--set-json", "config.connectionNamespaces=[]"); err != nil {
		t.Errorf("files with an empty list: %v; want it to render", err)
	}
}

// TestConnectionNamespacesSchema: each item is a namespace name, a DNS-1123 label of at most 63 characters, and the items are unique.
func TestConnectionNamespacesSchema(t *testing.T) {
	t.Parallel()
	helmPath(t)
	label63 := strings.Repeat("a", 63)
	for _, tc := range []struct {
		name string
		json string
		ok   bool
	}{
		{"a label", `["team-a"]`, true},
		{"digits first", `["1team"]`, true},
		{"63 characters", `["` + label63 + `"]`, true},
		{"64 characters", `["` + label63 + `a"]`, false},
		{"capitals", `["Team"]`, false},
		{"an underscore", `["team_a"]`, false},
		{"a dot", `["team.a"]`, false},
		{"a leading hyphen", `["-team"]`, false},
		{"a trailing hyphen", `["team-"]`, false},
		{"a wildcard", `["*"]`, false},
		{"a namespace/name", `["team-a/slack"]`, false},
		{"empty", `[""]`, false},
		{"a repeat", `["team-a","team-a"]`, false},
		{"a string, not a list", `"team-a"`, false},
		{"a number in the list", `[1]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", crMode("-f", values("crds"), "--set", "config.chatConnection=agent-kourier/slack-main", "--set-json", "config.connectionNamespaces="+tc.json)...)
			switch {
			case tc.ok && err != nil:
				t.Errorf("%s: %v; want it to render", tc.json, err)
			case !tc.ok && (err == nil || !strings.Contains(err.Error(), "config.connectionNamespaces")):
				t.Errorf("%s: render error = %v; want the schema to refuse config.connectionNamespaces", tc.json, err)
			}
		})
	}
}

// entryJSON is a config.chatConnections value of one ChatConnection named slack-main, in the namespace given (the release's when empty), whose
// credentials are in the Secret slack-main-creds.
func entryJSON(ns string) string {
	nsField := ""
	if ns != "" {
		nsField = `"namespace":"` + ns + `",`
	}
	return `config.chatConnections={"slack-main":{` + nsField + `"spec":{"platform":"slack","credentialsSecretRef":{"name":"slack-main-creds"}}}}`
}

// TestNoOwnConnection: with config.source crds, an empty config.chatConnection and a config.connectionNamespaces that holds a namespace is a
// broker with no ChatConnection of its own. The pod gets no AGENTKOURIER_CHAT_CONNECTION (and the list, right after the route), the chart grants no
// Secret for a connection it cannot see, a ChatConnection it renders must be in an admitted namespace (else it would be rendered and never served),
// and chatConnectionSecret, which names an own connection's Secret, is refused. The same list with config.chatConnection set changes nothing of the
// own connection, which keeps its variable and its rules.
func TestNoOwnConnection(t *testing.T) {
	t.Parallel()
	helmPath(t)
	const key = "AGENTKOURIER_CONNECTION_NAMESPACES"
	image := "image.repository=agent-kourier,config.source=crds"
	args := func(set string, json ...string) []string {
		a := crMode("--set", image+set)
		for _, j := range json {
			a = append(a, "--set-json", j)
		}
		return a
	}
	envNames := func(d appsv1.Deployment) []string {
		var names []string
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			names = append(names, e.Name)
		}
		return names
	}

	t.Run("a list alone: no variable for an own connection, no Secret granted", func(t *testing.T) {
		t.Parallel()
		objs := mustRender(t, "agent-kourier", "rel-ns", args("", `config.connectionNamespaces=["team-a","team-b"]`)...)
		d := deployment(t, objs)
		env := podEnv(d)
		if v, has := env["AGENTKOURIER_CHAT_CONNECTION"]; has {
			t.Errorf("AGENTKOURIER_CHAT_CONNECTION = %q; want it absent with no own connection", v)
		}
		if env["AGENTKOURIER_CONFIG_SOURCE"] != "crds" || env[key] != "team-a,team-b" {
			t.Errorf("source %q, %s %q; want crds and team-a,team-b", env["AGENTKOURIER_CONFIG_SOURCE"], key, env[key])
		}
		if names := envNames(d); len(names) < 2 || names[0] != "AGENTKOURIER_CONFIG_SOURCE" || names[1] != key {
			t.Errorf("environment %v; want %s right after AGENTKOURIER_CONFIG_SOURCE", names, key)
		}
		if got := secretRoles(t, objs); len(got) != 0 {
			t.Errorf("Secret Roles %v; want none (each team grants its own)", got)
		}
		for _, o := range objs {
			if o.kind() == "ChatConnection" {
				t.Errorf("rendered %s/%s; want no ChatConnection", o.namespace(), o.name())
			}
		}
	})

	t.Run("a rendered connection in an admitted namespace is rendered and granted by its own reference", func(t *testing.T) {
		t.Parallel()
		for _, ns := range []string{"", "team-a"} {
			home := ns
			if home == "" {
				home = "rel-ns"
			}
			objs := mustRender(t, "agent-kourier", "rel-ns", args("", entryJSON(ns), `config.connectionNamespaces=["`+home+`"]`)...)
			if _, has := podEnv(deployment(t, objs))["AGENTKOURIER_CHAT_CONNECTION"]; has {
				t.Errorf("namespace %q: AGENTKOURIER_CHAT_CONNECTION is rendered; want it absent", ns)
			}
			var cc v1alpha1.ChatConnection
			find(t, objs, "ChatConnection", "slack-main", &cc)
			if cc.Namespace != home {
				t.Errorf("ChatConnection namespace %q; want %q", cc.Namespace, home)
			}
			if got, want := secretRoles(t, objs), map[string][]string{home: {"slack-main-creds"}}; !equality.Semantic.DeepEqual(got, want) {
				t.Errorf("namespace %q: Secret Roles %v; want %v", ns, got, want)
			}
		}
	})

	t.Run("a rendered connection outside the list is refused, and the message names it", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, ns, want string }{
			{"the release namespace", "", `namespace "rel-ns"`},
			{"another namespace", "platform", `namespace "platform"`},
		} {
			_, err := render(t.Context(), t, "agent-kourier", "rel-ns", args("", entryJSON(tc.ns), `config.connectionNamespaces=["team-a"]`)...)
			if err == nil {
				t.Errorf("%s: rendered; want a refusal", tc.name)
				continue
			}
			for _, want := range []string{`"slack-main"`, tc.want, "config.connectionNamespaces", "never serve"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: render error = %v; want one containing %q", tc.name, err, want)
				}
			}
		}
	})

	t.Run("chatConnectionSecret is refused: there is no own connection for it to name", func(t *testing.T) {
		t.Parallel()
		_, err := render(t.Context(), t, "agent-kourier", "rel-ns", args(",config.chatConnectionSecret=slack-bot", `config.connectionNamespaces=["team-a"]`)...)
		if err == nil || !strings.Contains(err.Error(), "names the Secret of the broker's own ChatConnection, and there is none") {
			t.Errorf("render error = %v; want the refusal that says config.chatConnectionSecret names an own connection's Secret and there is none", err)
		}
	})

	t.Run("with config.chatConnection set the list keeps the own connection, and its entry need not be admitted", func(t *testing.T) {
		t.Parallel()
		objs := mustRender(t, "agent-kourier", "rel-ns", args(",config.chatConnection=platform/slack-main,config.chatConnectionSecret=slack-bot", entryJSON("platform"), `config.connectionNamespaces=["team-a"]`)...)
		env := podEnv(deployment(t, objs))
		if env["AGENTKOURIER_CHAT_CONNECTION"] != "platform/slack-main" || env[key] != "team-a" {
			t.Errorf("AGENTKOURIER_CHAT_CONNECTION %q, %s %q; want platform/slack-main and team-a", env["AGENTKOURIER_CHAT_CONNECTION"], key, env[key])
		}
	})

	t.Run("both empty with one rendered entry: the entry is the own connection, byte for byte as explicitly set", func(t *testing.T) {
		t.Parallel()
		for _, ns := range []string{"", "platform"} {
			home := ns
			if home == "" {
				home = "rel-ns"
			}
			implied, err := render(t.Context(), t, "agent-kourier", "rel-ns", args("", entryJSON(ns))...)
			if err != nil {
				t.Fatal(err)
			}
			explicit, err := render(t.Context(), t, "agent-kourier", "rel-ns", args(",config.chatConnection="+home+"/slack-main", entryJSON(ns))...)
			if err != nil {
				t.Fatal(err)
			}
			if implied != explicit {
				t.Errorf("namespace %q: the implied own connection renders differently from the same one set:\n%s", ns, firstDifference(explicit, implied))
			}
			if got := podEnv(deployment(t, decode(t, implied)))["AGENTKOURIER_CHAT_CONNECTION"]; got != home+"/slack-main" {
				t.Errorf("namespace %q: AGENTKOURIER_CHAT_CONNECTION %q; want %s/slack-main", ns, got, home)
			}
		}
	})

	t.Run("both empty with no entry serves every namespace; two entries are refused, and the message names every way out", func(t *testing.T) {
		t.Parallel()
		for name, a := range map[string][]string{"no entry": args(""), "an empty list": args("", "config.connectionNamespaces=[]")} {
			env := podEnv(deployment(t, mustRender(t, "agent-kourier", "rel-ns", a...)))
			for _, k := range []string{"AGENTKOURIER_CHAT_CONNECTION", key} {
				if v, has := env[k]; has {
					t.Errorf("%s: %s = %q; want it absent: nothing set is every namespace", name, k, v)
				}
			}
		}
		two := `config.chatConnections={"a":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"a"}}},"b":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"b"}}}}`
		_, err := render(t.Context(), t, "agent-kourier", "rel-ns", args("", two)...)
		if err == nil {
			t.Fatal("two entries: rendered; want a refusal")
		}
		for _, want := range []string{"needs config.chatConnection", "config.connectionNamespaces", "config.connectionNamespaceSelector", "config.source: files"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("two entries: render error = %v; want one containing %q", err, want)
			}
		}
	})
}
