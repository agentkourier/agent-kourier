package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// The file that init writes renders with the chart, and Agent Kourier loads what the chart renders: the check in init
// reads the values the way the chart's template does, and this test holds the two together. It needs helm, as the chart
// tests do, and is skipped without it (or fails when AGENTKOURIER_REQUIRE_HELM=1).
func TestInitValuesRenderWithTheChart(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("AGENTKOURIER_REQUIRE_HELM") == "1" {
			t.Fatal("helm is not on PATH, and AGENTKOURIER_REQUIRE_HELM=1 says the chart tests must run")
		}
		t.Skip("helm is not on PATH: this test renders charts/agent-kourier with `helm template` and is skipped without it (install helm, or set AGENTKOURIER_REQUIRE_HELM=1 to make this a failure)")
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"a2a", []string{"--dialect", "a2a"}},
		{"kagent-v1 with an agent token", []string{"--dialect", "kagent-v1", "--agent-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			args := append(starterArgs(out), tc.args...)
			if _, stderr, code := initRun(t, "", env{}.get, "", args...); code != exitOK {
				t.Fatalf("init: exit code %d, stderr %q", code, stderr)
			}
			rendered, err := exec.CommandContext(t.Context(), helm, "template", "agent-kourier", "../../charts/agent-kourier",
				"-n", "agent-kourier", "-f", out, "--set", "image.repository=registry.example.test/agent-kourier", "--set", "image.tag=dev",
				"--show-only", "templates/configmap.yaml").Output()
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			var cm struct {
				Data map[string]string `json:"data"`
			}
			if err := yaml.Unmarshal(rendered, &cm); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for name, body := range cm.Data {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := config.Load(t.Context(), dir)
			if err != nil {
				t.Fatalf("Agent Kourier does not load what the chart renders from init's file: %v", err)
			}
			if len(got.Bindings) != 1 || len(got.AgentBackends) != 1 || len(got.ChatConnections) != 1 {
				t.Fatalf("the chart rendered %d Bindings, %d AgentBackends and %d ChatConnections; want one of each", len(got.Bindings), len(got.AgentBackends), len(got.ChatConnections))
			}
			// What init checked is what the chart renders.
			want := loadWritten(t, out)
			if got.Bindings[0].Namespace != want.Bindings[0].Namespace || got.Bindings[0].Spec.Chat.Channel != want.Bindings[0].Spec.Chat.Channel ||
				(got.Bindings[0].Spec.Identity.TokenSecretRef == nil) != (want.Bindings[0].Spec.Identity.TokenSecretRef == nil) {
				t.Errorf("the chart's Binding is %+v; init checked %+v", got.Bindings[0], want.Bindings[0])
			}
		})
	}
}

// chartObjects are what the tests below read of a rendered chart.
type chartObjects struct {
	// secrets maps a namespace to the Secret names its Roles grant by name.
	secrets map[string][]string
	// env is the broker's environment, by name.
	env map[string]string
	// namespaceRules counts the ClusterRole rules that name namespaces.
	namespaceRules int
}

// renderChartWith renders the chart with the values file out and reads the Roles, the ClusterRole and the Deployment.
func renderChartWith(t *testing.T, helm, out string) chartObjects {
	t.Helper()
	rendered, err := exec.CommandContext(t.Context(), helm, "template", "agent-kourier", "../../charts/agent-kourier",
		"-n", "agent-kourier", "-f", out, "--set", "image.repository=registry.example.test/agent-kourier", "--set", "image.tag=dev").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	type object struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Rules []struct {
			Resources     []string `json:"resources"`
			ResourceNames []string `json:"resourceNames"`
		} `json:"rules"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string } `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	got := chartObjects{secrets: map[string][]string{}, env: map[string]string{}}
	for doc := range strings.SplitSeq(string(rendered), "\n---\n") {
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatal(err)
		}
		switch o.Kind {
		case "Role":
			for _, r := range o.Rules {
				got.secrets[o.Metadata.Namespace] = append(got.secrets[o.Metadata.Namespace], r.ResourceNames...)
			}
		case "ClusterRole":
			for _, r := range o.Rules {
				if slices.Contains(r.Resources, "namespaces") {
					got.namespaceRules++
				}
			}
		case "Deployment":
			for _, e := range o.Spec.Template.Spec.Containers[0].Env {
				got.env[e.Name] = e.Value
			}
		}
	}
	return got
}

// helmOrSkip returns the helm binary, or skips the test (fails it when AGENTKOURIER_REQUIRE_HELM=1) without one.
func helmOrSkip(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("AGENTKOURIER_REQUIRE_HELM") == "1" {
			t.Fatal("helm is not on PATH, and AGENTKOURIER_REQUIRE_HELM=1 says the chart tests must run")
		}
		t.Skip("helm is not on PATH: this test renders charts/agent-kourier with `helm template` and is skipped without it")
	}
	return helm
}

// The values that init writes for custom resources hold comments only, and the chart renders them into a broker with no
// ChatConnection of its own, no list and no selector, which serves every namespace, and no Role for a Secret: the team
// grants its own, as the commands init prints do. Like the test above it needs helm.
func TestInitCustomResourceValuesRenderWithTheChart(t *testing.T) {
	helm := helmOrSkip(t)
	for _, tc := range []struct {
		name       string
		agentToken bool
	}{{"without an agent token", false}, {"with an agent token", true}} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			args := crArgs(out)
			if tc.agentToken {
				args = append(args, "--agent-token")
			}
			if _, stderr, code := initRun(t, "", env{}.get, "", args...); code != exitOK {
				t.Fatalf("init: exit code %d, stderr %q", code, stderr)
			}
			assertNoValues(t, readFile(t, out))
			got := renderChartWith(t, helm, out)
			if len(got.secrets) != 0 {
				t.Errorf("the chart's Roles name the Secrets %v; want none, since the team grants its own", got.secrets)
			}
			if got.env["AGENTKOURIER_CONFIG_SOURCE"] != "crds" {
				t.Errorf("the broker's environment is %v; want the crds route", got.env)
			}
			for _, name := range []string{"AGENTKOURIER_CHAT_CONNECTION", "AGENTKOURIER_CONNECTION_NAMESPACES", "AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"} {
				if v, ok := got.env[name]; ok {
					t.Errorf("the broker's environment sets %s=%q; want it unset, which serves every namespace", name, v)
				}
			}
			if got.namespaceRules != 0 {
				t.Errorf("the ClusterRole has %d rules on namespaces; want none without a selector", got.namespaceRules)
			}
		})
	}
}

// The controls that init's comments show, uncommented, are values the chart takes: the list renders the team's namespace
// and the selector renders as JSON with the rule that lets the broker follow namespaces.
func TestInitCommentedControlsRenderWithTheChart(t *testing.T) {
	helm := helmOrSkip(t)
	out := outFile(t)
	if _, stderr, code := initRun(t, "", env{}.get, "", crArgs(out)...); code != exitOK {
		t.Fatalf("init: exit code %d, stderr %q", code, stderr)
	}
	// The controls are the comment lines from "# config:" to the end of the file; uncommenting strips "# " from each.
	text := readFile(t, out)
	head, controls, ok := strings.Cut(text, "\n# config:\n")
	if !ok {
		t.Fatalf("the values file does not show config commented out:\n%s", text)
	}
	var uncommented strings.Builder
	uncommented.WriteString("config:\n")
	for line := range strings.Lines(controls) {
		rest, ok := strings.CutPrefix(line, "#")
		if !ok {
			t.Fatalf("line %q after # config: is not a comment", line)
		}
		uncommented.WriteString(strings.TrimPrefix(rest, " "))
	}
	if err := os.WriteFile(out, []byte(head+"\n"+uncommented.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := renderChartWith(t, helm, out)
	if got.env["AGENTKOURIER_CONNECTION_NAMESPACES"] != pageTeam {
		t.Errorf("AGENTKOURIER_CONNECTION_NAMESPACES = %q; want %s", got.env["AGENTKOURIER_CONNECTION_NAMESPACES"], pageTeam)
	}
	if want := `{"matchLabels":{"agentkourier.dev/serve":"true"}}`; got.env["AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"] != want {
		t.Errorf("AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR = %q; want %s", got.env["AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"], want)
	}
	if _, own := got.env["AGENTKOURIER_CHAT_CONNECTION"]; own || got.namespaceRules != 1 {
		t.Errorf("own connection set: %v, namespaces rules: %d; want none and one", own, got.namespaceRules)
	}
}
