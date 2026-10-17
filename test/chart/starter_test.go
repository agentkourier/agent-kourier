package chart_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// starterValues is the first-install values file the docs tell a user to download and fill in.
const starterValues = "../../examples/helm/my-values.yaml"

// chartReadme is the chart's README, whose first YAML example shows the starter's resources inline.
const chartReadme = "../../charts/agent-kourier/README.md"

// The starter renders with the chart and Agent Kourier loads what it renders, as a user's install would: a field
// Agent Kourier does not know fails here, not in a crash-looping pod.
func TestStarterValuesRenderAndLoad(t *testing.T) {
	objs := mustRender(t, "agent-kourier", "agent-kourier", "-f", starterValues,
		"--set", "image.repository=registry.example.test/agent-kourier", "--set", "image.tag=dev")
	dir := t.TempDir()
	files := 0
	for _, o := range objs {
		if o.kind() != "ConfigMap" {
			continue
		}
		var cm struct {
			Data map[string]string `json:"data"`
		}
		find(t, objs, "ConfigMap", o.name(), &cm)
		for name, body := range cm.Data {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			files++
		}
	}
	if files == 0 {
		t.Fatal("the starter rendered no resource files")
	}
	set, err := config.Load(t.Context(), dir)
	if err != nil {
		t.Fatalf("Agent Kourier does not load the starter's resources: %v", err)
	}
	if len(set.Bindings) != 1 || set.Bindings[0].Name != "my-binding" {
		t.Errorf("Bindings = %v, want the one Binding my-binding", set.Bindings)
	}
}

// The chart README's inline example is the starter without its comments, so the two cannot drift apart.
func TestChartReadmeExampleIsTheStarter(t *testing.T) {
	readme, err := os.ReadFile(chartReadme)
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(readme), "\n```yaml\n")
	if !ok {
		t.Fatalf("%s has no YAML example", chartReadme)
	}
	example, _, ok := strings.Cut(block, "\n```\n")
	if !ok {
		t.Fatalf("%s: the YAML example is not closed", chartReadme)
	}
	starter, err := os.ReadFile(starterValues)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := yaml.Unmarshal([]byte(example), &got); err != nil {
		t.Fatalf("%s example: %v", chartReadme, err)
	}
	if err := yaml.Unmarshal(starter, &want); err != nil {
		t.Fatalf("%s: %v", starterValues, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the chart README's example is not the starter's resources:\nREADME:  %v\nstarter: %v", got, want)
	}
}
