package kindmanifests_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/test/e2e"
)

// promptAgentManifest is the fast tier's prompt agent (examples/recipes/prompt-agent), its fake model and its MCP server, in namespace e2e.
const promptAgentManifest = "../../hack/kind/fast/prompt-agent/prompt-agent.yaml"

// promptAgentDockerfile builds the fake model's and the MCP server's images from the recipe's directory.
const promptAgentDockerfile = "../../hack/kind/fast/prompt-agent/Dockerfile"

// The Services of the three, as the Binding and the agent's own config reach them.
const (
	promptAgentURL = "http://prompt-agent.e2e.svc.cluster.local:8080"
	fakeModelURL   = "http://prompt-fake-model.e2e.svc.cluster.local:8080/v1"
	promptMCPURL   = "http://prompt-mcp.e2e.svc.cluster.local:8080/mcp"
)

// TestThePromptAgentBindingReachesTheRecipesAgentWithItsToken: the prompt agent's channel speaks the a2a dialect to the agent's Service at
// its exact URL, sends the token of Secret prompt-agent-bearer, and lets the agent ask, with a timeout short enough for the scenario that
// leaves a question to time out. The agent fails closed without a token, so the Binding must send one.
func TestThePromptAgentBindingReachesTheRecipesAgentWithItsToken(t *testing.T) {
	set, err := config.Load(t.Context(), fastConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	rb, ok := snap.BindingForChannel("agent-kourier", "slack-fast", e2e.ChannelPromptAgent)
	if !ok {
		t.Fatalf("channel %s resolves to no Binding", e2e.ChannelPromptAgent)
	}
	if b := rb.Backend.Spec; rb.Backend.Name != "prompt-a2a" || b.Dialect != v1alpha1.DialectA2A || b.URL != promptAgentURL || len(b.Headers) != 0 {
		t.Errorf("channel %s goes to backend %s at %s (%s) with %d headers; want prompt-a2a, the a2a dialect at %s and no headers",
			e2e.ChannelPromptAgent, rb.Backend.Name, b.URL, b.Dialect, len(b.Headers), promptAgentURL)
	}
	spec := rb.Binding.Spec
	if ref := spec.Identity.TokenSecretRef; ref == nil || ref.Name != "prompt-agent-bearer" {
		t.Errorf("the prompt agent's Binding has the token reference %+v; want Secret prompt-agent-bearer, the token the agent checks", ref)
	}
	if spec.Agent.Name != "prompt-agent" || spec.Agent.Namespace != "e2e" {
		t.Errorf("the prompt agent's Binding names the agent %s/%s; want e2e/prompt-agent", spec.Agent.Namespace, spec.Agent.Name)
	}
	if !spec.Interactions.AskUser || spec.Interactions.ToolApprovals || spec.Interactions.Timeout.Duration != 20*time.Second {
		t.Errorf("the prompt agent's Binding has askUser %t, toolApprovals %t and timeout %s; want askUser, no approvals and 20s",
			spec.Interactions.AskUser, spec.Interactions.ToolApprovals, spec.Interactions.Timeout.Duration)
	}
	if spec.Chat.Output != v1alpha1.OutputLive {
		t.Errorf("the prompt agent's Binding has output %q; want live, so that its reply streams", spec.Chat.Output)
	}
}

// TestThePromptAgentRunsOnTheFakeModelWithItsMCPServerAndChecksTheToken: the tier's agent is the recipe's image with an agent.yaml of the
// tier's own: model openai-chat:fake at the fake model's Service, one MCP server (the recipe's test one) with the one tool the scenarios
// call. It mounts the bearer from the Secret the Binding reads, and nothing turns the token check off.
func TestThePromptAgentRunsOnTheFakeModelWithItsMCPServerAndChecksTheToken(t *testing.T) {
	var agent, fake, mcp appsv1.Deployment
	var cm corev1.ConfigMap
	services := map[string]corev1.Service{}
	for _, doc := range objects(t, promptAgentManifest) {
		raw, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if meta, _ := doc["metadata"].(map[string]any); meta["namespace"] != "e2e" {
			t.Errorf("%s %v is in namespace %v; want e2e, with the other doubles", doc["kind"], meta["name"], meta["namespace"])
		}
		switch doc["kind"] {
		case "Deployment":
			var d appsv1.Deployment
			if err := yaml.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			switch d.Name {
			case "prompt-agent":
				agent = d
			case "prompt-fake-model":
				fake = d
			case "prompt-mcp":
				mcp = d
			default:
				t.Errorf("unexpected Deployment %s", d.Name)
			}
		case "Service":
			var s corev1.Service
			if err := yaml.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			services[s.Name] = s
		case "ConfigMap":
			if err := yaml.Unmarshal(raw, &cm); err != nil {
				t.Fatal(err)
			}
		default:
			t.Errorf("%s has a %v; want Deployments, Services and a ConfigMap only (the Secrets are lib.sh's, never committed)", promptAgentManifest, doc["kind"])
		}
	}
	for name, d := range map[string]appsv1.Deployment{"prompt-agent": agent, "prompt-fake-model": fake, "prompt-mcp": mcp} {
		if len(d.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("Deployment %s has %d containers; want one", name, len(d.Spec.Template.Spec.Containers))
		}
		s, ok := services[name]
		if !ok || s.Spec.Type != "" && s.Spec.Type != corev1.ServiceTypeClusterIP || len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 8080 {
			t.Errorf("Service %s is %+v; want a ClusterIP Service on 8080, published nowhere, like the other doubles", name, s.Spec)
		}
		if c := d.Spec.Template.Spec.Containers[0]; c.ImagePullPolicy != corev1.PullNever {
			t.Errorf("Deployment %s pulls with %q; want Never: kind loads the image lib.sh builds", name, c.ImagePullPolicy)
		}
	}
	// The images are the ones lib.sh builds and loads.
	lib := readHackKind(t, "lib.sh")
	for name, d := range map[string]appsv1.Deployment{"PROMPT_AGENT_IMAGE": agent, "PROMPT_FAKE_MODEL_IMAGE": fake, "PROMPT_MCP_IMAGE": mcp} {
		want := name + "=" + d.Spec.Template.Spec.Containers[0].Image
		if !strings.Contains(lib, "\n"+want+"\n") {
			t.Errorf("lib.sh does not set %s: the Deployment runs an image that lib.sh does not build", want)
		}
	}

	c := agent.Spec.Template.Spec.Containers[0]
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if got := env["OPENAI_BASE_URL"].Value; got != fakeModelURL {
		t.Errorf("the agent's OPENAI_BASE_URL is %q; want the fake model's Service, %s", got, fakeModelURL)
	}
	if _, ok := env["PROMPT_AGENT_ALLOW_ANONYMOUS"]; ok {
		t.Error("the agent sets PROMPT_AGENT_ALLOW_ANONYMOUS: the tier proves the token check, so it must stay on")
	}
	tokenFile := env["PROMPT_AGENT_TOKEN_FILE"].Value
	mounted := false
	for _, m := range c.VolumeMounts {
		for _, v := range agent.Spec.Template.Spec.Volumes {
			if v.Name == m.Name && v.Secret != nil && v.Secret.SecretName == "prompt-agent-bearer" && filepath.Dir(tokenFile) == filepath.Clean(m.MountPath) && filepath.Base(tokenFile) == "token" {
				mounted = true
			}
		}
	}
	if !mounted {
		t.Errorf("PROMPT_AGENT_TOKEN_FILE is %q; want the key token of Secret prompt-agent-bearer, mounted", tokenFile)
	}
	mcpToken := env["OPS_MCP_TOKEN"].ValueFrom
	var serverToken *corev1.EnvVarSource
	for _, e := range mcp.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "MCP_TOKEN" {
			serverToken = e.ValueFrom
		}
	}
	if mcpToken == nil || serverToken == nil || mcpToken.SecretKeyRef == nil || serverToken.SecretKeyRef == nil ||
		mcpToken.SecretKeyRef.Name != "prompt-agent-mcp" || serverToken.SecretKeyRef.Name != "prompt-agent-mcp" {
		t.Errorf("the agent's OPS_MCP_TOKEN is %+v and the MCP server's MCP_TOKEN %+v; want both from Secret prompt-agent-mcp", mcpToken, serverToken)
	}

	// The agent's own config: the fake model, and the MCP server with the one tool the scenarios call.
	var spec struct {
		Model        string `json:"model"`
		Capabilities []struct {
			MCP struct {
				URL                string   `json:"url"`
				AuthorizationToken string   `json:"authorization_token"`
				AllowedTools       []string `json:"allowed_tools"`
			} `json:"MCP"`
		} `json:"capabilities"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["agent.yaml"]), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Model != "openai-chat:fake" || len(spec.Capabilities) != 1 {
		t.Fatalf("the tier's agent.yaml is %+v; want model openai-chat:fake and one MCP server", spec)
	}
	if m := spec.Capabilities[0].MCP; m.URL != promptMCPURL || m.AuthorizationToken != "Bearer ${OPS_MCP_TOKEN}" || !slices.Equal(m.AllowedTools, []string{"runbook"}) {
		t.Errorf("the tier's MCP entry is %+v; want %s with the token from OPS_MCP_TOKEN and allowed_tools [runbook]", m, promptMCPURL)
	}
	mountsConfig := false
	for _, v := range agent.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == cm.Name {
			mountsConfig = true
		}
	}
	if !mountsConfig || env["PROMPT_AGENT_SPEC"].Value != "/etc/prompt-agent/agent.yaml" {
		t.Errorf("the agent mounts ConfigMap %s: %t, and reads %q; want it mounted and read at /etc/prompt-agent/agent.yaml", cm.Name, mountsConfig, env["PROMPT_AGENT_SPEC"].Value)
	}
}

// TestTheFakeModelAndMCPImagesShareTheRecipesPythonImage: the tier's Dockerfile builds every target FROM the build argument PYTHON_IMAGE, with
// no default, and deploy_prompt_agent passes the one image of the recipe's Dockerfile, so that the digest is pinned in one place, which
// Renovate bumps.
func TestTheFakeModelAndMCPImagesShareTheRecipesPythonImage(t *testing.T) {
	raw, err := os.ReadFile(promptAgentDockerfile)
	if err != nil {
		t.Fatal(err)
	}
	from := regexp.MustCompile(`(?m)^FROM (\S+)`).FindAllStringSubmatch(string(raw), -1)
	if len(from) != 2 {
		t.Fatalf("the tier's Dockerfile has %d FROM lines; want two, the fake model's and the MCP server's", len(from))
	}
	for _, m := range from {
		if m[1] != "${PYTHON_IMAGE}" {
			t.Errorf("the tier's Dockerfile builds FROM %s; want ${PYTHON_IMAGE}", m[1])
		}
	}
	if !regexp.MustCompile(`(?m)^ARG PYTHON_IMAGE$`).Match(raw) {
		t.Error("the tier's Dockerfile does not declare ARG PYTHON_IMAGE, without a default")
	}
	// The recipe's .dockerignore lets in only what the recipe's image copies; BuildKit reads Dockerfile.dockerignore, beside this Dockerfile, in its
	// place, and it must let in what this one copies, and nothing else.
	copied := regexp.MustCompile(`(?m)^COPY (.+) \./$`).FindAllStringSubmatch(string(raw), -1)
	var want []string
	for _, m := range copied {
		want = append(want, strings.Fields(m[1])...)
	}
	ignore, err := os.ReadFile(promptAgentDockerfile + ".dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	for _, line := range strings.Split(string(ignore), "\n") {
		if name, ok := strings.CutPrefix(line, "!"); ok {
			allowed = append(allowed, name)
		}
	}
	slices.Sort(want)
	slices.Sort(allowed)
	if !strings.Contains(string(ignore), "\n*\n") || !slices.Equal(allowed, want) {
		t.Errorf("Dockerfile.dockerignore lets in %v over *; want exactly what the Dockerfile copies, %v", allowed, want)
	}
	recipe, err := os.ReadFile("../../examples/recipes/prompt-agent/agent/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(regexp.MustCompile(`(?m)^FROM `).FindAll(recipe, -1)); n != 1 {
		t.Errorf("the recipe's Dockerfile has %d FROM lines; deploy_prompt_agent reads the image of the one", n)
	}
	lib := readHackKind(t, "lib.sh")
	for _, want := range []string{`python=$(sed -n 's/^FROM \([^ ]*\).*/\1/p' "${recipe}/Dockerfile")`, `--target fake-model -f "${HERE}/fast/prompt-agent/Dockerfile" --build-arg "PYTHON_IMAGE=${python}"`, `--target mcp -f "${HERE}/fast/prompt-agent/Dockerfile" --build-arg "PYTHON_IMAGE=${python}"`} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib.sh's deploy_prompt_agent lacks %q", want)
		}
	}
}

// TestUpFastBringsUpThePromptAgentWithOneBearerInBothNamespaces: up-fast.sh runs lib.sh's step for the prompt agent, which makes the
// Secret prompt-agent-bearer in e2e (the agent mounts it) and in agent-kourier (the Binding reads it) from one value, so the two agree.
func TestUpFastBringsUpThePromptAgentWithOneBearerInBothNamespaces(t *testing.T) {
	if !strings.Contains(readHackKind(t, "up-fast.sh"), "\ndeploy_prompt_agent\n") {
		t.Error("up-fast.sh does not call deploy_prompt_agent")
	}
	lib := readHackKind(t, "lib.sh")
	for _, want := range []string{
		`-n e2e create secret generic prompt-agent-bearer --from-literal=token="${PROMPT_AGENT_BEARER}"`,
		`-n agent-kourier create secret generic prompt-agent-bearer --from-literal=token="${PROMPT_AGENT_BEARER}"`,
		`-n e2e create secret generic prompt-agent-mcp --from-literal=token=`,
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib.sh lacks %q", want)
		}
	}
	if strings.Contains(readHackKind(t, "up-full.sh"), "deploy_prompt_agent") {
		t.Error("up-full.sh brings up the prompt agent: its Binding is the fast tier's alone")
	}
}

// readHackKind reads a file of hack/kind.
func readHackKind(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile("../../hack/kind/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
