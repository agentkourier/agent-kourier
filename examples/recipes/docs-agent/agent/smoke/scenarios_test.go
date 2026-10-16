package smoke_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/smoke"
)

const scenariosGlob = "../../evals/scenarios/*.yaml"

// scenario is the shape of the recipe's evals/scenarios files (see test/recipes for what every recipe's share).
type scenario struct {
	Name     string   `yaml:"name"`
	App      string   `yaml:"app"`
	Question string   `yaml:"question"`
	Cause    string   `yaml:"cause"`
	Evidence []string `yaml:"evidence"`
}

func loadScenarios(t *testing.T) map[string]scenario {
	t.Helper()
	files, err := filepath.Glob(scenariosGlob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenario files at %s: %v", scenariosGlob, err)
	}
	out := map[string]scenario{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		var sc scenario
		if err := dec.Decode(&sc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[filepath.Base(f)] = sc
	}
	return out
}

// TestScenariosAgainstTheRepositoryDocs checks, without a model, the part of each scenario that a program can: for a
// question the documentation answers, the page named as evidence exists and the keyword search puts a page named as
// evidence among its first five hits; for a question it does not answer, the term named as absent is on no page.
// What the agent then makes of the pages is for a person to read against the scenario's cause.
func TestScenariosAgainstTheRepositoryDocs(t *testing.T) {
	ix, err := corpus.Load(t.Context(), repoDocs, corpus.Options{Exclude: smoke.DocsExclude})
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	var text strings.Builder
	for _, p := range ix.Paths() {
		known[p] = true
		page, err := ix.Read(t.Context(), p, "")
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(strings.ToLower(page.Text))
		text.WriteString("\n")
	}
	var answerable, unanswerable int
	for file, sc := range loadScenarios(t) {
		t.Run(file, func(t *testing.T) {
			var pages, absent []string
			for _, e := range sc.Evidence {
				switch {
				case strings.HasPrefix(e, "absent: "):
					absent = append(absent, strings.TrimPrefix(e, "absent: "))
				case strings.HasSuffix(e, ".md") && !strings.Contains(e, " "):
					pages = append(pages, e)
				}
			}
			if len(pages) > 0 && len(absent) > 0 {
				t.Fatalf("evidence names pages and an absent term; a scenario is one or the other")
			}
			if len(pages) == 0 && len(absent) == 0 {
				t.Fatalf("evidence names neither a page nor an absent term")
			}
			for _, p := range pages {
				if !known[p] {
					t.Errorf("evidence page %s is not in the corpus", p)
				}
			}
			for _, term := range absent {
				if strings.Contains(text.String(), strings.ToLower(term)) {
					t.Errorf("the corpus holds %q, so the question is answerable", term)
				}
			}
			if len(absent) > 0 {
				unanswerable++
				return
			}
			answerable++
			hits, err := ix.Search(t.Context(), sc.Question, 5)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hits {
				for _, p := range pages {
					if h.Path == p {
						return
					}
				}
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.Path)
			}
			t.Errorf("search for %q found %v, none of the evidence pages %v", sc.Question, got, pages)
		})
	}
	if answerable < 5 || unanswerable < 2 {
		t.Errorf("scenarios: %d answerable and %d unanswerable, want at least 5 and 2", answerable, unanswerable)
	}
}
