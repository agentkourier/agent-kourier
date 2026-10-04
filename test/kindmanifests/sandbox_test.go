package kindmanifests_test

import (
	"encoding/json"
	"fmt"
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
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// These tests pin the sandbox tier's manifests (hack/kind/sandbox): what it applies to the full tier's cluster to dogfood
// courier on a real Slack workspace and a real model. sandboxDir is in sandboxenv_test.go.

// manifestFiles are the Kubernetes manifests of the sandbox, which must parse; some carry a placeholder that up.sh fills in.
var manifestFiles = []string{"faults.yaml", "tools-rbac.yaml", "agent.yaml", "token-refresh.yaml", "courier/deployment.yaml",
	"courier/config/chatconnection.yaml", "courier/config/binding.yaml", "courier/alert-trial/alert-binding.yaml", "modelconfig-anthropic.yaml", "modelconfig-openai.yaml", "modelconfig-bedrock.yaml"}

// sandboxObjects returns every object of the given kind in the sandbox's file, as a map of its name.
func sandboxObjects[T any](t *testing.T, file, kind string) map[string]T {
	t.Helper()
	out := map[string]T{}
	for _, doc := range objects(t, sandboxDir+file) {
		if doc["kind"] != kind {
			continue
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var obj T
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("%s: %s: %v", file, kind, err)
		}
		out[doc["metadata"].(map[string]any)["name"].(string)] = obj
	}
	if len(out) == 0 {
		t.Fatalf("%s has no %s", file, kind)
	}
	return out
}

// rendered is a manifest of the sandbox with its placeholders filled in as up.sh does.
func rendered(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(sandboxDir + file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer("__MODEL_NAME__", "claude-fake-model", "__MODEL_REGION__", "us-east-1", "__SLACK_CHANNEL_ID__", "C0SANDBOX01", "__ALERT_CHANNEL_ID__", trialChannel).Replace(string(raw))
}

func TestSandboxManifestsParse(t *testing.T) {
	for _, file := range manifestFiles {
		docs := objects(t, sandboxDir+file)
		if len(docs) == 0 {
			t.Errorf("%s holds no object", file)
		}
		for _, doc := range docs {
			if doc["apiVersion"] == nil || doc["kind"] == nil || doc["metadata"] == nil {
				t.Errorf("%s: an object without apiVersion, kind or metadata: %v", file, doc)
			}
		}
	}
	// The placeholders are each in exactly the files that up.sh renders, and the rendered files hold none.
	for _, tc := range []struct{ file, placeholder string }{{"modelconfig-anthropic.yaml", "__MODEL_NAME__"}, {"modelconfig-openai.yaml", "__MODEL_NAME__"}, {"modelconfig-bedrock.yaml", "__MODEL_NAME__"}, {"modelconfig-bedrock.yaml", "__MODEL_REGION__"}, {"courier/config/binding.yaml", "__SLACK_CHANNEL_ID__"}, {"courier/alert-trial/alert-binding.yaml", "__ALERT_CHANNEL_ID__"}} {
		raw, err := os.ReadFile(sandboxDir + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		// Once as a value (the comments say what it is), and none left after rendering.
		if len(regexp.MustCompile(`(?m)^\s+\w+: `+tc.placeholder+`$`).FindAll(raw, -1)) != 1 || strings.Contains(rendered(t, tc.file), "__") {
			t.Errorf("%s: want %s as one value, and nothing of the kind after rendering", tc.file, tc.placeholder)
		}
	}
}

// TestSandboxHoldsNoSecret: no Secret is in the repository for this tier, and no manifest carries a token-shaped value:
// up.sh makes both Secrets from the secrets file.
func TestSandboxHoldsNoSecret(t *testing.T) {
	tokenShaped := regexp.MustCompile(`xox[bpa]-|xapp-|sk-[A-Za-z0-9]|sk-ant-`)
	entries, err := filepath.Glob(sandboxDir + "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob(sandboxDir + "courier/*.yaml")
	more2, _ := filepath.Glob(sandboxDir + "courier/config/*.yaml")
	more3, _ := filepath.Glob(sandboxDir + "courier/alert-trial/*.yaml")
	for _, file := range slices.Concat(entries, more, more2, more3) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if tokenShaped.Match(raw) {
			t.Errorf("%s holds something shaped like a token", file)
		}
		if strings.HasSuffix(file, "slack-app-manifest.yaml") {
			continue
		}
		for _, doc := range objects(t, file) {
			if doc["kind"] == "Secret" {
				t.Errorf("%s declares a Secret: they are made from the secrets file, never committed", file)
			}
		}
	}
}

// sandboxConfig loads the sandbox's courier config as up.sh builds the ConfigMap: its files, rendered, and the full tier's
// AgentBackend, which is the verifying front door.
func sandboxConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, file := range []string{"courier/config/chatconnection.yaml", "courier/config/binding.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(file)), []byte(rendered(t, file)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("../../hack/kind/full/courier/agentbackend.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agentbackend.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSandboxBindingAndConnection: one Binding, the sandbox channel to sandbox-sre through the front door, live, with
// askUser; one ChatConnection, Slack over Socket Mode, on the Secret up.sh makes.
func TestSandboxBindingAndConnection(t *testing.T) {
	set, err := config.Load(t.Context(), sandboxConfig(t))
	if err != nil || len(set.Bindings) != 1 || len(set.ChatConnections) != 1 || len(set.AgentBackends) != 1 {
		t.Fatalf("Load = %+v, %v; want one Binding, one ChatConnection and one AgentBackend", set, err)
	}
	snap, err := config.Resolve(t.Context(), set, secrets{"courier/sandbox-slack": {"botToken", "appToken"}, "courier/courier-agent-token": {"token"}})
	if err != nil {
		t.Fatal(err)
	}
	rb, ok := snap.BindingForChannel("courier", "sandbox-slack", "C0SANDBOX01")
	if !ok {
		t.Fatal("the sandbox channel resolves to no Binding")
	}
	spec := rb.Binding.Spec
	if spec.Agent.Namespace != "kagent" || spec.Agent.Name != "sandbox-sre" || spec.Chat.Output != v1alpha1.OutputLive || !spec.Interactions.AskUser || spec.Interactions.ToolApprovals {
		t.Errorf("Binding agent %s/%s, output %q, askUser %t, toolApprovals %t; want kagent/sandbox-sre, live, askUser on and tool approvals off",
			spec.Agent.Namespace, spec.Agent.Name, spec.Chat.Output, spec.Interactions.AskUser, spec.Interactions.ToolApprovals)
	}
	// The verifying front door (4180), never the controller (8083), which does not verify tokens (spec D32, R4).
	if rb.Backend.Spec.URL != "http://kagent-frontdoor.kagent.svc.cluster.local:4180" || rb.Backend.Spec.Dialect != "kagent-v1" {
		t.Errorf("backend %q (%s); want the front door on 4180, dialect kagent-v1", rb.Backend.Spec.URL, rb.Backend.Spec.Dialect)
	}
	if spec.Identity.TokenSecretRef == nil || spec.Identity.TokenSecretRef.Name != "courier-agent-token" {
		t.Errorf("identity %+v; want the Dex token Secret courier-agent-token, which the refresh keeps fresh", spec.Identity)
	}
	conn := rb.Connection.Spec
	if conn.Platform != "slack" || conn.Mode != v1alpha1.ModeSocket || conn.CredentialsSecretRef.Name != "sandbox-slack" {
		t.Errorf("connection %+v; want Slack over Socket Mode on the Secret sandbox-slack", conn)
	}
	// Without the Secrets, as before they are made, the config does not resolve.
	if _, err := config.Resolve(t.Context(), set, secrets{"courier/courier-agent-token": {"token"}}); err == nil {
		t.Error("config resolved without the Slack Secret")
	}
}

// TestSandboxCourier: the production image, not the testhooks one; no test hook; the pod label the front door trusts;
// a Deployment and a volume of its own, so that the full tier's courier and the sandbox's never share one.
func TestSandboxCourier(t *testing.T) {
	d := sandboxObjects[appsv1.Deployment](t, "courier/deployment.yaml", "Deployment")["courier-sandbox"]
	pod := d.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("%d containers, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != "courier:dev" || c.ImagePullPolicy != corev1.PullNever || strings.Contains(c.Image, "testhooks") {
		t.Errorf("image %q with pull policy %s; want the production image courier:dev, loaded and never pulled", c.Image, c.ImagePullPolicy)
	}
	for _, e := range c.Env {
		if strings.Contains(e.Name, "SLACK") || e.ValueFrom != nil || strings.Contains(e.Value, "fakeslack") {
			t.Errorf("env %s: the production image has no Slack API override, and nothing comes from a Secret by env", e.Name)
		}
	}
	if d.Spec.Template.Labels["app.kubernetes.io/name"] != "courier" || d.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != "sandbox" ||
		d.Spec.Template.Labels["app.kubernetes.io/instance"] != "sandbox" {
		t.Errorf("labels %v, selector %v; want app.kubernetes.io/name=courier (the front door's NetworkPolicy trusts it) and instance=sandbox, which the full tier's courier lacks",
			d.Spec.Template.Labels, d.Spec.Selector.MatchLabels)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *d.Spec.Replicas != 1 {
		t.Errorf("strategy %s, %d replicas; want Recreate and one: two pods must never share the SQLite volume", d.Spec.Strategy.Type, *d.Spec.Replicas)
	}
	if pod.ServiceAccountName != "courier" {
		t.Errorf("ServiceAccount %q; want courier, whose Role lists and watches Secrets in its namespace", pod.ServiceAccountName)
	}
	sc := c.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || pod.SecurityContext == nil || !*pod.SecurityContext.RunAsNonRoot || *pod.SecurityContext.RunAsUser != 65532 {
		t.Errorf("security contexts %+v and %+v; want non-root 65532, a read-only root, no escalation", pod.SecurityContext, sc)
	}
	vols := map[string]corev1.VolumeSource{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v.VolumeSource
	}
	if vols["config"].ConfigMap == nil || vols["config"].ConfigMap.Name != "courier-sandbox-config" || vols["data"].PersistentVolumeClaim == nil || vols["data"].PersistentVolumeClaim.ClaimName != "courier-sandbox-data" {
		t.Errorf("volumes %+v; want the sandbox's own ConfigMap and PVC, never the full tier's courier-config and courier-data", vols)
	}
	pvc := sandboxObjects[corev1.PersistentVolumeClaim](t, "courier/deployment.yaml", "PersistentVolumeClaim")["courier-sandbox-data"]
	if pvc.Namespace != "courier" || *pvc.Spec.StorageClassName != "standard" {
		t.Errorf("PVC %s on %v; want courier-sandbox-data in courier on the standard class", pvc.Name, pvc.Spec.StorageClassName)
	}
	// The same listen address, port and probes as the full tier's, so that the metrics port is the same.
	fast := appsv1.Deployment{}
	load(t, courierDir+"deployment.yaml", "Deployment", &fast)
	if c.StartupProbe.HTTPGet.Path != "/healthz" || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.Ports[0].ContainerPort != fast.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort {
		t.Errorf("probes %+v, %+v on ports %+v; want the fast tier's", c.StartupProbe, c.ReadinessProbe, c.Ports)
	}
}

// TestSandboxToolsAreBoundToView: the tool server's ServiceAccount is bound to the built-in view ClusterRole and to nothing
// else, the chart's own cluster-admin RBAC is off, and the server runs read-only, on the k8s tools alone.
func TestSandboxToolsAreBoundToView(t *testing.T) {
	crb := sandboxObjects[rbacv1.ClusterRoleBinding](t, "tools-rbac.yaml", "ClusterRoleBinding")["sandbox-tools-view"]
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != "view" || len(crb.Subjects) != 1 ||
		crb.Subjects[0].Kind != "ServiceAccount" || crb.Subjects[0].Name != "sandbox-tools" || crb.Subjects[0].Namespace != "kagent" {
		t.Errorf("binding %+v; want the ServiceAccount kagent/sandbox-tools bound to the view ClusterRole alone", crb)
	}
	for _, doc := range objects(t, sandboxDir+"tools-rbac.yaml") {
		if k := doc["kind"]; k != "ClusterRoleBinding" {
			t.Errorf("tools-rbac.yaml holds a %v; want only the binding to view", k)
		}
	}
	raw, err := os.ReadFile(sandboxDir + "tools-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		FullnameOverride string `json:"fullnameOverride"`
		RBAC             struct {
			Create bool `json:"create"`
		} `json:"rbac"`
		UseDefaultServiceAccount bool `json:"useDefaultServiceAccount"`
		Tools                    struct {
			EnabledTools []string `json:"enabledTools"`
			Args         []string `json:"args"`
			Image        struct {
				Tag string `json:"tag"`
			} `json:"image"`
		} `json:"tools"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.FullnameOverride != "sandbox-tools" || v.RBAC.Create || v.UseDefaultServiceAccount {
		t.Errorf("values name %q, rbac.create %t, useDefaultServiceAccount %t; want the ServiceAccount sandbox-tools and none of the chart's cluster-admin RBAC",
			v.FullnameOverride, v.RBAC.Create, v.UseDefaultServiceAccount)
	}
	if !slices.Equal(v.Tools.EnabledTools, []string{"k8s"}) || !slices.Equal(v.Tools.Args, []string{"--read-only"}) {
		t.Errorf("tools %v with args %v; want the k8s tools alone, with --read-only", v.Tools.EnabledTools, v.Tools.Args)
	}
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(v.Tools.Image.Tag) {
		t.Errorf("image tag %q; want it pinned by digest", v.Tools.Image.Tag)
	}
	// The Service the RemoteMCPServer names is the chart's, from the same override.
	rm := sandboxObjects[map[string]any](t, "agent.yaml", "RemoteMCPServer")["sandbox-tools"]
	if url := rm["spec"].(map[string]any)["url"]; url != "http://sandbox-tools.kagent.svc.cluster.local:8084/mcp" {
		t.Errorf("RemoteMCPServer url %v; want the sandbox-tools Service on the server's port", url)
	}
}

// readToolsTheServerLeaves are the nine tools that `tool-server --read-only --tools k8s` serves (0.3.0), which an MCP
// tools/list gave when this was written. The agent lists some of them and no other.
var readToolsTheServerLeaves = []string{"k8s_describe_resource", "k8s_generate_resource", "k8s_get_available_api_resources", "k8s_get_cluster_configuration",
	"k8s_get_events", "k8s_get_pod_logs", "k8s_get_resource_yaml", "k8s_get_resources", "k8s_wait"}

// writeWord matches a tool name that does, or may, change something.
var writeWord = regexp.MustCompile(`apply|create|delete|patch|scale|rollout|annotate|label|exec|remove|generate|wait|execute|install|upgrade|rm_|set_|update|edit|replace|restart|cordon|drain`)

type sandboxAgent struct {
	Spec struct {
		Template struct {
			ModelConfig  struct{ Name string } `json:"modelConfig"`
			SystemPrompt string                `json:"systemPrompt"`
			Tools        []struct {
				MCP struct {
					Server struct{ Kind, Name string } `json:"server"`
					Tools  []string                    `json:"tools"`
				} `json:"mcp"`
				SubAgent any `json:"subAgent"`
			} `json:"tools"`
		} `json:"template"`
		Harness struct {
			Workload struct{ Image string } `json:"workload"`
		} `json:"harness"`
	} `json:"spec"`
}

func TestSandboxAgentListsOnlyReadTools(t *testing.T) {
	a := sandboxObjects[sandboxAgent](t, "agent.yaml", "Agent")["sandbox-sre"]
	tools := a.Spec.Template.Tools
	if len(tools) != 1 || tools[0].SubAgent != nil || tools[0].MCP.Server.Kind != "RemoteMCPServer" || tools[0].MCP.Server.Name != "sandbox-tools" {
		t.Fatalf("tools %+v; want one source, the sandbox tool server", tools)
	}
	listed := tools[0].MCP.Tools
	if len(listed) == 0 {
		t.Fatal("the agent lists no tool by name: an MCP source with no list serves all of them")
	}
	for _, name := range listed {
		if !slices.Contains(readToolsTheServerLeaves, name) {
			t.Errorf("tool %s is not one the read-only server serves", name)
		}
		if writeWord.MatchString(name) {
			t.Errorf("tool %s looks like one that changes something", name)
		}
		if !strings.HasPrefix(name, "k8s_get_") && name != "k8s_describe_resource" {
			t.Errorf("tool %s: want get, describe, logs, events and the like", name)
		}
	}
	for _, name := range []string{"k8s_get_resources", "k8s_describe_resource", "k8s_get_pod_logs", "k8s_get_events"} {
		if !slices.Contains(listed, name) {
			t.Errorf("the agent lacks %s, which investigating a fault needs", name)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(listed)))) != len(listed) {
		t.Errorf("tools %v repeat a name", listed)
	}
}

func TestSandboxAgent(t *testing.T) {
	a := sandboxObjects[sandboxAgent](t, "agent.yaml", "Agent")["sandbox-sre"]
	if a.Spec.Template.ModelConfig.Name != "sandbox-model" {
		t.Errorf("model %q; want the ModelConfig sandbox-model", a.Spec.Template.ModelConfig.Name)
	}
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(a.Spec.Harness.Workload.Image) {
		t.Errorf("harness image %q; want it pinned by digest", a.Spec.Harness.Workload.Image)
	}
	prompt := strings.ToLower(a.Spec.Template.SystemPrompt)
	for _, want := range []string{"never change anything", "read-only", "what you checked", "checked:", "ask_user", "shop"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the system prompt does not say %q", want)
		}
	}
	// The agent is the one the Binding names, and the model's Secret is the one up.sh makes.
	for _, file := range []string{"modelconfig-anthropic.yaml", "modelconfig-openai.yaml", "modelconfig-bedrock.yaml"} {
		mc := map[string]any{}
		if err := yaml.Unmarshal([]byte(rendered(t, file)), &mc); err != nil {
			t.Fatal(err)
		}
		spec := mc["spec"].(map[string]any)
		want := map[string]string{"modelconfig-anthropic.yaml": "Anthropic", "modelconfig-openai.yaml": "OpenAI", "modelconfig-bedrock.yaml": "Bedrock"}[file]
		if mc["metadata"].(map[string]any)["name"] != "sandbox-model" || spec["provider"] != want || spec["model"] != "claude-fake-model" || spec["apiKeySecret"] != "sandbox-model" {
			t.Errorf("%s: %v; want provider %s, the model name filled in, and the key from the Secret sandbox-model", file, spec, want)
		}
		if file == "modelconfig-bedrock.yaml" {
			// Bedrock's key is the Secret key that kagent fixes (AWS_BEARER_TOKEN_BEDROCK), so the ModelConfig names none; the region is required.
			if _, named := spec["apiKeySecretKey"]; named || spec["bedrock"].(map[string]any)["region"] != "us-east-1" {
				t.Errorf("%s: %v; want a region and no apiKeySecretKey", file, spec)
			}
		} else if spec["apiKeySecretKey"] != "apiKey" {
			t.Errorf("%s: %v; want the key apiKey", file, spec)
		}
		// A real provider, not the mock model, and no key in the file.
		for k := range spec {
			if strings.Contains(strings.ToLower(k), "baseurl") || (k == "defaultHeaders" && file != "modelconfig-bedrock.yaml") { // Bedrock's one header is pinned in sandboxbedrock_test.go
				t.Errorf("%s sets %s; the sandbox model is the provider's own API", file, k)
			}
		}
		if raw, _ := yaml.Marshal(spec); strings.Contains(string(raw), "mockllm") {
			t.Errorf("%s names the mock model", file)
		}
	}
}

// TestSandboxFaults: every fault is in namespace shop, and the one healthy app is.
func TestSandboxFaults(t *testing.T) {
	deps := sandboxObjects[appsv1.Deployment](t, "faults.yaml", "Deployment")
	svcs := sandboxObjects[corev1.Service](t, "faults.yaml", "Service")
	if got := slices.Sorted(maps.Keys(deps)); !slices.Equal(got, []string{"catalog", "checkout", "frontend", "inventory", "recommender", "search"}) {
		t.Fatalf("deployments %v; want checkout, catalog, recommender, inventory, search and frontend", got)
	}
	ns := sandboxObjects[corev1.Namespace](t, "faults.yaml", "Namespace")
	if _, ok := ns["shop"]; !ok || len(ns) != 1 {
		t.Errorf("namespaces %v; want shop alone", slices.Sorted(maps.Keys(ns)))
	}
	for name, d := range deps {
		if d.Namespace != "shop" {
			t.Errorf("%s is in %q, not shop", name, d.Namespace)
		}
		// Nothing says which fault is which: the agent has to find out.
		for k, v := range d.Labels {
			if !slices.Contains([]string{"app", "app.kubernetes.io/part-of"}, k) || strings.ContainsAny(v, " ") {
				t.Errorf("%s has label %s=%s; labels must not give a fault away", name, k, v)
			}
		}
		if len(d.Annotations) != 0 {
			t.Errorf("%s has annotations %v", name, d.Annotations)
		}
	}
	container := func(name string) corev1.Container { return deps[name].Spec.Template.Spec.Containers[0] }

	// checkout: a crash loop with a telling log line.
	if cmd := strings.Join(container("checkout").Command, " "); !strings.Contains(cmd, "FATAL: cannot resolve payments-db.shop.svc.cluster.local") || !strings.Contains(cmd, "exit 1") {
		t.Errorf("checkout's command %q; want a telling FATAL line and exit 1", cmd)
	}
	if _, ok := svcs["payments-db"]; ok {
		t.Error("a payments-db Service exists: it is what checkout cannot find")
	}
	// catalog: a tag that does not exist, which is the one image that is not pinned by digest.
	if img := container("catalog").Image; !regexp.MustCompile(`^busybox:1\.37\.99$`).MatchString(img) {
		t.Errorf("catalog image %q; want the bad tag busybox:1.37.99", img)
	}
	// recommender: allocates without end under a small memory limit.
	rc := container("recommender")
	if lim := rc.Resources.Limits.Memory(); lim.Value() > 64<<20 || !strings.Contains(strings.Join(rc.Command, " "), "while True") {
		t.Errorf("recommender limit %v, command %v; want a limit of at most 64Mi and a loop that allocates", lim, rc.Command)
	}
	// inventory: healthy pods, and a Service that selects none of them.
	inv := svcs["inventory"]
	podLabels := deps["inventory"].Spec.Template.Labels
	if len(inv.Spec.Selector) == 0 || selects(inv.Spec.Selector, podLabels) {
		t.Errorf("inventory's selector %v matches its pods' labels %v; want it to match nothing", inv.Spec.Selector, podLabels)
	}
	for name, d := range deps {
		if selects(inv.Spec.Selector, d.Spec.Template.Labels) {
			t.Errorf("inventory's selector matches the pods of %s", name)
		}
	}
	if p := container("inventory").ReadinessProbe; p == nil || p.HTTPGet.Path != "/" {
		t.Errorf("inventory readiness %+v; want a probe its server passes, so that the Service's selector is the fault", p)
	}
	// search: a readiness probe on a path its server does not have.
	if p := container("search").ReadinessProbe; p == nil || p.HTTPGet.Path != "/ready" || strings.Contains(strings.Join(container("search").Command, " "), "ready/") {
		t.Errorf("search readiness %+v; want a probe on /ready, which busybox httpd answers with 404", p)
	}
	// frontend: fine, and says so with a probe that passes.
	if p := container("frontend").ReadinessProbe; p == nil || p.HTTPGet.Path != "/" {
		t.Errorf("frontend readiness %+v; want a passing probe", p)
	}
	if !selects(svcs["frontend"].Spec.Selector, deps["frontend"].Spec.Template.Labels) {
		t.Error("the frontend's Service must select its pods")
	}
	for name, d := range deps {
		sc := d.Spec.Template.Spec.Containers[0].SecurityContext
		if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || *d.Spec.Template.Spec.SecurityContext.RunAsNonRoot == false {
			t.Errorf("%s: want a non-root, read-only, no-escalation container", name)
		}
		if d.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().IsZero() {
			t.Errorf("%s has no memory limit: the tier's node is small", name)
		}
	}
}

// selects reports whether a Service selector matches the labels.
func selects(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// imageRef finds the images a manifest file names, as `image:` values and the `tag:` of a chart's image.
var imageRef = regexp.MustCompile(`(?m)^\s*(?:- )?(?:image|tag): "?([^\s"#]+)`)

// TestSandboxImagesArePinned: every image the sandbox names is pinned by digest, except the one fault that is a bad tag and
// courier:dev, which is built here and loaded, never pulled.
func TestSandboxImagesArePinned(t *testing.T) {
	exempt := map[string]string{"busybox:1.37.99": "catalog's bad tag, which is its fault", "courier:dev": "the image built by make image and loaded into the cluster"}
	seen := 0
	for _, file := range append(slices.Clone(manifestFiles), "tools-values.yaml") {
		raw, err := os.ReadFile(sandboxDir + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range imageRef.FindAllStringSubmatch(string(raw), -1) {
			seen++
			if _, ok := exempt[m[1]]; ok {
				continue
			}
			if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(m[1]) {
				t.Errorf("%s: image %q is not pinned by digest", file, m[1])
			}
		}
	}
	if seen < 8 {
		t.Errorf("found %d image references; the pattern has stopped finding them", seen)
	}
	// Only catalog's bad tag, in the faults, is outside the pins.
	for name, d := range sandboxObjects[appsv1.Deployment](t, "faults.yaml", "Deployment") {
		img := d.Spec.Template.Spec.Containers[0].Image
		if !strings.Contains(img, "@sha256:") && name != "catalog" {
			t.Errorf("%s runs %q, unpinned; only catalog's is allowed to be", name, img)
		}
	}
}

// TestSandboxTokenRefresh: a CronJob keeps the Binding's Dex token fresh from inside the cluster, with a ServiceAccount that can
// patch that one Secret and nothing else, and a pod that does not carry the courier label the front door trusts.
func TestSandboxTokenRefresh(t *testing.T) {
	cj := sandboxObjects[batchv1.CronJob](t, "token-refresh.yaml", "CronJob")["courier-token-refresh"]
	role := sandboxObjects[rbacv1.Role](t, "token-refresh.yaml", "Role")["courier-token-refresh"]
	if len(role.Rules) != 1 || fmt.Sprint(role.Rules[0].Resources, role.Rules[0].Verbs, role.Rules[0].ResourceNames) != "[secrets] [patch] [courier-agent-token]" {
		t.Errorf("rules %+v; want patch on the Secret courier-agent-token and nothing else", role.Rules)
	}
	rb := sandboxObjects[rbacv1.RoleBinding](t, "token-refresh.yaml", "RoleBinding")["courier-token-refresh"]
	pod := cj.Spec.JobTemplate.Spec.Template
	if rb.RoleRef.Name != role.Name || rb.Subjects[0].Name != pod.Spec.ServiceAccountName {
		t.Errorf("the Role is bound to %v, and the pod runs as %s", rb.Subjects, pod.Spec.ServiceAccountName)
	}
	if _, ok := pod.Labels["app.kubernetes.io/name"]; ok {
		t.Errorf("the pod carries labels %v; it must not carry the courier label the front door trusts", pod.Labels)
	}
	// Hourly (the token lasts 24 hours, and a sleeping laptop swallows runs), no overlap, a bounded run.
	if cj.Spec.Schedule != "0 * * * *" || cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent || cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds == nil {
		t.Errorf("schedule %q, concurrency %s; want hourly, Forbid, and a deadline", cj.Spec.Schedule, cj.Spec.ConcurrencyPolicy)
	}
	script := strings.Join(pod.Spec.Containers[0].Command, "\n")
	for _, want := range []string{"http://dex.dex.svc.cluster.local:5556/dex/token", "client_id=courier-binding", "merge-patch+json", "secrets/courier-agent-token", "--data-binary @-", "-o /dev/null", "curl -sf"} {
		if !strings.Contains(script, want) {
			t.Errorf("the refresh script lacks %q", want)
		}
	}
	// The token is never printed. Every line that mentions it is one of three (the assignment from Dex, the shape check, and
	// the encoding into the pipe to the PATCH), and every line that writes anywhere is one of three: the two fixed
	// messages and that pipe. So nothing else can print, log or file the token, whatever it is written as.
	mentionsToken := regexp.MustCompile(`\$\{?tok\b|\btok=`)
	writes := regexp.MustCompile(`\becho\b|\bprintf\b|\bcat\b|\btee\b|>&|/dev/(stdout|stderr|tty)|\s>\s|>>`)
	const (
		assign = `tok=$(curl -sf -m 20 -d grant_type=password -d client_id=courier-binding -d client_secret=fake-courier-binding-secret \`
		check  = `case "${tok}" in`
		encode = `printf '{"data":{"token":"%s"}}' "$(printf %s "${tok}" | base64 | tr -d '\n')" |`
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

// TestSandboxMakeTargets: the three targets exist, are in `make` help, and are in the README's list.
func TestSandboxMakeTargets(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"sandbox-up", "sandbox-down", "sandbox-heal"} {
		if !regexp.MustCompile(`(?m)^` + target + `: .*## \S`).Match(mk) {
			t.Errorf("the Makefile has no %s target with a help line", target)
		}
		if !strings.Contains(string(readme), "make "+target) {
			t.Errorf("the README does not list make %s", target)
		}
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\bsandbox-up\b.*\bsandbox-down\b.*\bsandbox-heal\b`).Match(mk) {
		t.Error("the sandbox targets are not .PHONY")
	}
	if !strings.Contains(string(readme), "hack/kind/sandbox/slack-app-manifest.yaml") {
		t.Error("the README does not point at the Slack app manifest")
	}
}

// TestSandboxUpWaitsForCourierToSayItIsConnected: a Ready pod says nothing of Slack (readiness is not tied to it), so up.sh
// claims a connection only after the log line the Slack adapter writes for it, which the README names with the gauge.
func TestSandboxUpWaitsForCourierToSayItIsConnected(t *testing.T) {
	up, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), "'"+slack.ConnectedMessage+"'") {
		t.Errorf("up.sh does not wait for the line %q, which the Slack adapter logs when Socket Mode connects", slack.ConnectedMessage)
	}
	if strings.Contains(string(up), "courier is Ready: connected to Slack") {
		t.Error("up.sh still claims a Ready pod is connected to Slack")
	}
	for _, want := range []string{slack.ConnectedMessage, "courier_chat_connected"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("the README's \"where to look\" does not name %q", want)
		}
	}
}

// TestSandboxScriptsParse: bash -n on every script of the sandbox.
func TestSandboxScriptsParse(t *testing.T) {
	scripts, err := filepath.Glob(sandboxDir + "*.sh")
	if err != nil || len(scripts) < 4 {
		t.Fatalf("scripts %v, %v; want up, down, check-env and env", scripts, err)
	}
	for _, script := range scripts {
		out, err := exec.CommandContext(t.Context(), "bash", "-n", script).CombinedOutput()
		if err != nil {
			t.Errorf("bash -n %s: %v\n%s", script, err, out)
		}
	}
}

// TestSandboxUpChecksTheSecretsFirst: with a file that is refused, up.sh stops before it asks Docker, kind, kubectl or helm
// anything: stand-ins for them, first in PATH, leave a mark if they are run.
func TestSandboxUpChecksTheSecretsFirst(t *testing.T) {
	bin := t.TempDir()
	mark := filepath.Join(bin, "ran")
	for _, tool := range []string{"docker", "kind", "kubectl", "helm", "jq"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho "+tool+" >> "+mark+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bad := envFile(t, 0o644, goodEnv()...) // world-readable
	cmd := exec.CommandContext(t.Context(), "bash", sandboxDir+"up.sh")
	cmd.Env = append(os.Environ(), "SANDBOX_ENV="+bad, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("up.sh accepted a world-readable file:\n%s", out)
	}
	if !strings.Contains(string(out), "chmod 600") {
		t.Errorf("up.sh said %q; want the mode refused", out)
	}
	noLeak(t, "up.sh output", string(out))
	if _, err := os.Stat(mark); err == nil {
		t.Error("up.sh ran a tool before the secrets file was accepted")
	}
}
