package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
