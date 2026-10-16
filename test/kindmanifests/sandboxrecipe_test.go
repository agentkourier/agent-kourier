package kindmanifests_test

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// These tests pin how the sandbox tier uses the sre-triage recipe (examples/recipes/sre-triage): the recipe owns the
// agent's manifests, the tier's scripts apply them from there, and the recipe's quality scenarios cover its faults.
// agentDir and agentFiles are in sandbox_test.go.

// TestTheAgentManifestsLiveOnlyInTheRecipe: each of the agent's files is in the recipe and not also in the sandbox
// directory, where a second copy would drift from the first.
func TestTheAgentManifestsLiveOnlyInTheRecipe(t *testing.T) {
	for _, file := range agentFiles {
		if _, err := os.Stat(agentDir + file); err != nil {
			t.Errorf("%s: %v; want it in the recipe", file, err)
		}
		if _, err := os.Stat(sandboxDir + file); err == nil {
			t.Errorf("%s is in %s as well as in the recipe", file, sandboxDir)
		}
	}
}

// TestTheScriptsApplyTheAgentFromTheRecipe: up.sh applies the faults, the tool server's binding and values, the prompt
// and the agent from the recipe's directory, and down.sh deletes the agent, its prompt and the binding from there;
// neither names those files in the sandbox directory, and the directory each script sets as AGENT is the recipe's.
func TestTheScriptsApplyTheAgentFromTheRecipe(t *testing.T) {
	abs, err := filepath.Abs(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		script string
		uses   []string // the files the script reads from ${AGENT}
	}{
		{"up.sh", agentFiles},
		{"down.sh", []string{"agent.yaml", "agent-prompt.yaml", "tools-rbac.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.script, func(t *testing.T) {
			raw, err := os.ReadFile(sandboxDir + tt.script)
			if err != nil {
				t.Fatal(err)
			}
			s := string(raw)
			if got, err := filepath.EvalSymlinks(scriptAgentDir(t, tt.script)); err != nil || got != want {
				t.Errorf("%s sets AGENT to %q (%v); want the recipe's directory %s", tt.script, got, err, want)
			}
			for _, file := range tt.uses {
				if !strings.Contains(s, "${AGENT}/"+file) {
					t.Errorf("%s does not use ${AGENT}/%s", tt.script, file)
				}
			}
			for _, file := range agentFiles {
				if strings.Contains(s, "${SBX}/"+file) {
					t.Errorf("%s still reads ${SBX}/%s, which is in the recipe", tt.script, file)
				}
			}
		})
	}
}

// pathVar matches the assignments in a script that decide where AGENT points: HERE, ROOT, SBX and AGENT itself.
var pathVar = regexp.MustCompile(`(?m)^(HERE|ROOT|SBX|AGENT)=.*$`)

// scriptAgentDir returns the directory that the sandbox script sets as AGENT, absolute: it runs the script's own
// assignments of HERE, ROOT, SBX and AGENT in bash, with the script's path standing for its own, and so follows
// whichever way the script derives the directory.
func scriptAgentDir(t *testing.T, script string) string {
	t.Helper()
	abs, err := filepath.Abs(sandboxDir + script)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	lines := pathVar.FindAllString(string(raw), -1)
	if n := len(slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, "AGENT=") })); n != 1 {
		t.Fatalf("%s assigns AGENT %d times; want once", script, n)
	}
	// The script's own path reaches the assignments through the environment, so that no quoting of it is needed.
	prog := "set -eu\n" + strings.ReplaceAll(strings.Join(lines, "\n"), "${BASH_SOURCE[0]}", "${SCRIPT_PATH}") + "\nprintf %s \"${AGENT}\""
	cmd := exec.CommandContext(t.Context(), "bash", "-c", prog)
	cmd.Env = append(os.Environ(), "SCRIPT_PATH="+abs)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: resolving AGENT: %v\n%s", script, err, out)
	}
	return string(out)
}

// TestDownRemovesThePromptAfterTheAgent: down.sh deletes everything that up.sh applies for the agent, and the prompt
// goes after the agent that selects it. Each delete tolerates an object that is already gone.
func TestDownRemovesThePromptAfterTheAgent(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "down.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	var at []int
	for _, file := range []string{"agent.yaml", "agent-prompt.yaml"} {
		line := regexp.MustCompile(`(?m)^"\$\{K\[@\]\}" delete -f "\$\{AGENT\}/` + regexp.QuoteMeta(file) + `".*$`).FindString(s)
		if line == "" {
			t.Fatalf("down.sh does not delete ${AGENT}/%s", file)
		}
		if !strings.Contains(line, "--ignore-not-found") {
			t.Errorf("down.sh deletes %s without --ignore-not-found: %s", file, line)
		}
		at = append(at, strings.Index(s, line))
	}
	if at[0] > at[1] {
		t.Error("down.sh deletes the prompt before the agent that selects it")
	}
}

// sreTriageScenario is the part of a scenario that the sandbox's faults decide: the Deployment it is about and the
// sample alert that starts it. The shape of a scenario is checked by go test ./test/recipes.
type sreTriageScenario struct {
	App   string `json:"app"`
	Alert string `json:"alert"`
}

// TestSreTriageScenariosCoverTheFaults: one scenario for each Deployment of faults.yaml, the healthy app included, and
// no other; each alert is the sample alert that the sandbox fires for that app.
func TestSreTriageScenariosCoverTheFaults(t *testing.T) {
	scenarios := filepath.Join(agentDir, "..", "evals", "scenarios")
	files, err := filepath.Glob(filepath.Join(scenarios, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("scenarios in %s: %v, %v; want one file for each fault", scenarios, files, err)
	}
	samples := loadSamples(t)
	apps := map[string]string{} // app to the file of its scenario
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var sc sreTriageScenario
		if err := yaml.Unmarshal(raw, &sc); err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if other, dup := apps[sc.App]; dup {
			t.Errorf("%s and %s are both about %s", file, other, sc.App)
		}
		apps[sc.App] = file
		if sc.Alert == "" {
			continue
		}
		p, ok := samples[sc.Alert]
		switch {
		case !ok:
			t.Errorf("%s: alert %q is not a sample alert in the sandbox's alerts.json", file, sc.Alert)
		case p.App != sc.App && p.Labels["service"] != sc.App:
			t.Errorf("%s: alert %s is about app %q and Service %q, not %s", file, sc.Alert, p.App, p.Labels["service"], sc.App)
		}
	}
	deps := sandboxObjects[appsv1.Deployment](t, "faults.yaml", "Deployment")
	if got, want := slices.Sorted(maps.Keys(apps)), slices.Sorted(maps.Keys(deps)); !slices.Equal(got, want) {
		t.Errorf("scenarios are about %v; want one for each Deployment of faults.yaml: %v", got, want)
	}
}
