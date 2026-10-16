package recipes_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// scenariosDir is where a recipe keeps its quality scenarios, relative to the recipe.
var scenariosDir = filepath.Join("evals", "scenarios")

// scenario is one of a recipe's quality scenarios (provisional data: nothing runs them yet). This is the shape every
// recipe's scenario files share; a recipe's own tests may check more of it, such as whether App names something that
// exists in the recipe's agent manifests.
type scenario struct {
	// Name says what the case is about.
	Name string `json:"name"`
	// App is the workload the case is about.
	App string `json:"app"`
	// Alert is the name of the alert that starts the case; Question is a person's question instead. Exactly one is set.
	Alert    string `json:"alert,omitempty"`
	Question string `json:"question,omitempty"`
	// Cause is what a good answer must name.
	Cause string `json:"cause"`
	// Evidence is what a good answer cites.
	Evidence []string `json:"evidence"`
}

// TestScenarios checks the scenario files of every recipe under examples/recipes. A recipe with none is not an
// error: the scenarios are optional.
func TestScenarios(t *testing.T) {
	recipes, err := recipeDirs(recipesDir)
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, dir := range recipes {
		files, err := filepath.Glob(filepath.Join(dir, scenariosDir, "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			checked++
			t.Run(filepath.Base(dir)+"/"+filepath.Base(file), func(t *testing.T) {
				if err := checkScenarioFile(file); err != nil {
					t.Error(err)
				}
			})
		}
	}
	t.Logf("checked %d scenario files under %s", checked, recipesDir)
}

// TestCheckScenarioFile runs the checker on fixtures: scenarios it must accept, and scenarios it must refuse, each
// with a message that names the file and the field at fault.
func TestCheckScenarioFile(t *testing.T) {
	tests := []struct {
		file string
		want []string // substrings the error must contain; none means the file must pass
	}{
		{file: "valid-alert.yaml"},
		{file: "valid-question.yaml"},
		{file: "unknown-field.yaml", want: []string{"unknown-field.yaml", "evidance: unknown field"}},
		{file: "missing-fields.yaml", want: []string{"missing-fields.yaml", "a name", "an app", "a cause", "some evidence"}},
		{file: "both-alert-and-question.yaml", want: []string{"both-alert-and-question.yaml", "exactly one of alert and question"}},
		{file: "neither-alert-nor-question.yaml", want: []string{"neither-alert-nor-question.yaml", "exactly one of alert and question"}},
		{file: "empty-evidence-item.yaml", want: []string{"empty-evidence-item.yaml", "an evidence item is empty"}},
		{file: "blank-fields.yaml", want: []string{"blank-fields.yaml", "a name", "an app", "an evidence item is empty"}},
		{file: "not-a-mapping.yaml", want: []string{"not-a-mapping.yaml"}},
		{file: "does-not-exist.yaml", want: []string{"does-not-exist.yaml", "no such file"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			err := checkScenarioFile(filepath.Join("testdata", "scenarios", tt.file))
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("checkScenarioFile: %v; want it to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkScenarioFile passed; want an error containing %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(filepath.ToSlash(err.Error()), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// checkScenarioFile reads the scenario at path and reports every way it does not have the shape of a scenario: a
// field it does not have, a name, an app, a cause or evidence that is missing or only whitespace, an empty evidence item, or neither or
// both of alert and question. Each problem names the file.
func checkScenarioFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read scenario: %w", err)
	}
	var sc scenario
	if err := config.DecodeStrict(raw, &sc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var problems []error
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	if blank(sc.Name) || blank(sc.App) || blank(sc.Cause) || len(sc.Evidence) == 0 {
		problems = append(problems, fmt.Errorf("%s: want a name, an app, a cause and some evidence, got %+v", path, sc))
	}
	if slices.ContainsFunc(sc.Evidence, blank) {
		problems = append(problems, fmt.Errorf("%s: an evidence item is empty", path))
	}
	if (sc.Alert == "") == (sc.Question == "") {
		problems = append(problems, fmt.Errorf("%s: want exactly one of alert and question, got alert %q and question %q", path, sc.Alert, sc.Question))
	}
	return errors.Join(problems...)
}
