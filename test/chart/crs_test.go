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

// TestDefaultsWithNoConfigAreRefusedWithAWayOut: a first install that gives the chart nothing to serve has no ChatConnection for the broker, and the
// render says so and names the route that needs no resource of its own, instead of rendering a pod that exits at start.
func TestDefaultsWithNoConfigAreRefusedWithAWayOut(t *testing.T) {
	t.Parallel()
	helmPath(t)
	_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", "--kube-version", "v1.33.2", "--set", "image.repository=agent-kourier")
	if err == nil {
		t.Fatal("the chart rendered an install with no values; want a refusal that names config.chatConnection")
	}
	for _, want := range []string{"needs config.chatConnection", "config.source: files"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("render error = %v; want one containing %q", err, want)
		}
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
			[]string{"needs Kubernetes 1.33 or later", "set config.source: files"}},
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

// TestDocumentedDefaultRouteInstallsGrantTheConnectionsSecret: the install the chart README and Install with Helm give for the default route names the
// ChatConnection and applies the resources themselves, so the chart sees no Secret to name. Their values must carry the grant (rbac.secretNames), or the
// render has no Role for the Secret and the broker, which cannot read its own ChatConnection's Secret, exits with code 3 in a loop.
func TestDocumentedDefaultRouteInstallsGrantTheConnectionsSecret(t *testing.T) {
	t.Parallel()
	helmPath(t)
	install, err := os.ReadFile("../../docs/how-to/install-with-helm.md")
	if err != nil {
		t.Fatal(err)
	}
	// The Secret the documented ChatConnection reads, from the resources the page applies.
	m := regexp.MustCompile(`(?s)kind: ChatConnection\n.*?credentialsSecretRef: \{name: ([a-z0-9-]+)\}`).FindSubmatch(install)
	if m == nil {
		t.Fatal("Install with Helm has no ChatConnection with a credentialsSecretRef")
	}
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
		{"no connection named, and none given", crMode("--set", image+",config.source=crds"), "needs config.chatConnection"},
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
	for _, set := range []struct{ name, route string }{{"config.source unset", ""}, {"config.source crds set", ",config.source=crds"}} {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"no connection named", []string{"--set", image + set.route}},
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
// defaults render. (A release made with those values at the defaults is the same render.) Measured for real on kind, from the released chart.
func TestUpgradeFromAReleaseThatLacksTheNewValues(t *testing.T) {
	t.Parallel()
	helmPath(t)
	base := []string{"-f", values("minimal")}
	want, err := render(t.Context(), t, "agent-kourier", "agent-kourier", base...)
	if err != nil {
		t.Fatal(err)
	}
	for _, unset := range []string{"crds", "config.source", "config.chatConnection", "rbac.secretNames", "crds.install"} {
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
	got, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), "--set", "config.source=files,config.chatConnection=agent-kourier/slack-main",
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
