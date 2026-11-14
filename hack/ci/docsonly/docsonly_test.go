package docsonly

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// workflowsDir is .github/workflows, from this package.
const workflowsDir = "../../../.github/workflows"

// gated names, per workflow, the jobs that have the docs-only step. A job is added here on purpose: gitleaks, lint and
// zizmor must always run in full.
var gated = map[string][]string{
	"ci.yml":       {"crd-corpus", "docs-agent", "echo-agent", "test", "test-postgres", "test-testhooks"},
	"codeql.yml":   {"analyze"},
	"security.yml": {"govulncheck", "image"},
}

// The two steps every gated job starts with, in this order.
const (
	wantCheckout = `
uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
with:
  persist-credentials: false
  fetch-depth: ${{ github.event_name == 'pull_request' && 2 || 1 }}
`
	wantDocsOnly = `
id: docs-only
name: Classify the pull request's changes
if: ${{ github.event_name == 'pull_request' }}
continue-on-error: true
env:
  PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
run: |
  scope=$(hack/ci/docs-only.sh)
  echo "${scope}" >>"${GITHUB_OUTPUT}"
`
)

// gate is the condition, in a step's if, that skips the step on a docs-only pull request.
const gate = "steps.docs-only.outputs.skip != 'true'"

// pushOnly is an if that holds only on a push; outcomeOf finds the steps whose outcome an if requires to be success.
var (
	pushOnly  = "github.event_name == 'push'"
	outcomeOf = regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outcome == 'success'`)
)

type step map[string]any

type workflow struct {
	Jobs map[string]struct {
		Steps []step `json:"steps"`
	} `json:"jobs"`
}

// check returns what is wrong with the docs-only gate of the workflows (file name to YAML): a gated job without the
// two steps as wanted, first, or a job not in gated with the docs-only step, or a later step that a pull request
// runs without the gate. A step runs only on a push when its if holds only on a push, or requires the success of a
// step that runs only on a push.
func check(workflows map[string][]byte) ([]string, error) {
	var checkout, docsOnly step
	if err := yaml.Unmarshal([]byte(wantCheckout), &checkout); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(wantDocsOnly), &docsOnly); err != nil {
		return nil, err
	}
	var problems []string
	for _, file := range slices.Sorted(maps.Keys(workflows)) {
		var wf workflow
		if err := yaml.Unmarshal(workflows[file], &wf); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
			steps := wf.Jobs[name].Steps
			at := slices.IndexFunc(steps, func(s step) bool { return s["id"] == "docs-only" })
			want := slices.Contains(gated[file], name)
			switch {
			case at < 0 && !want:
				continue
			case at < 0:
				problems = append(problems, fmt.Sprintf("%s: job %s has no docs-only step", file, name))
				continue
			case !want:
				problems = append(problems, fmt.Sprintf("%s: job %s has a docs-only step but is not one of the gated jobs", file, name))
				continue
			case at != 1:
				problems = append(problems, fmt.Sprintf("%s: job %s: the docs-only step is step %d, not the second", file, name, at+1))
				continue
			}
			if !reflect.DeepEqual(steps[0], checkout) {
				problems = append(problems, fmt.Sprintf("%s: job %s: the first step is not the checkout every gated job has", file, name))
			}
			if !reflect.DeepEqual(steps[1], docsOnly) {
				problems = append(problems, fmt.Sprintf("%s: job %s: the docs-only step is not the one every gated job has", file, name))
			}
			pushIDs := map[string]bool{}
			for i, s := range steps[2:] {
				cond, _ := s["if"].(string)
				id, _ := s["id"].(string)
				onPushOnly := strings.Contains(cond, pushOnly)
				for _, m := range outcomeOf.FindAllStringSubmatch(cond, -1) {
					onPushOnly = onPushOnly || pushIDs[m[1]]
				}
				switch {
				case onPushOnly:
					if id != "" {
						pushIDs[id] = true
					}
				case !strings.Contains(cond, gate):
					problems = append(problems, fmt.Sprintf("%s: job %s: step %d runs on a pull request without the docs-only gate", file, name, i+3))
				}
			}
		}
	}
	return problems, nil
}

// readWorkflows reads the workflows that gated names.
func readWorkflows(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for file := range gated {
		b, err := os.ReadFile(filepath.Join(workflowsDir, file))
		if err != nil {
			t.Fatal(err)
		}
		out[file] = b
	}
	return out
}

// TestWorkflowsGateAlike: every gated job of the repository's workflows has the gate as wanted, and no other job has it.
func TestWorkflowsGateAlike(t *testing.T) {
	problems, err := check(readWorkflows(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestCheckFindsDrift: each way a copy of the gate can drift is reported.
func TestCheckFindsDrift(t *testing.T) {
	repo := readWorkflows(t)
	for _, tt := range []struct {
		name, old, new, want string
	}{
		{"a docs-only step without continue-on-error", "        continue-on-error: true\n", "", "the docs-only step is not the one"},
		{"a docs-only step without the head", "          PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}\n", "          PR_HEAD_SHA: ''\n", "the docs-only step is not the one"},
		{"a checkout of depth 1", "          fetch-depth: ${{ github.event_name == 'pull_request' && 2 || 1 }}\n", "", "the first step is not the checkout"},
		{"a step without the gate", "      - if: ${{ steps.docs-only.outputs.skip != 'true' }}\n        run: make test-testhooks\n", "      - run: make test-testhooks\n", "runs on a pull request without the docs-only gate"},
		{"a gated job without the step", "      - id: docs-only\n", "      - id: docs-only-renamed\n", "has no docs-only step"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := maps.Clone(repo)
			if !strings.Contains(string(wfs["ci.yml"]), tt.old) {
				t.Fatalf("ci.yml has no %q to change", tt.old)
			}
			wfs["ci.yml"] = []byte(strings.Replace(string(wfs["ci.yml"]), tt.old, tt.new, 1))
			problems, err := check(wfs)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("problems = %q, want one with %q", problems, tt.want)
			}
		})
	}
	t.Run("an ungated job with the step", func(t *testing.T) {
		wfs := maps.Clone(repo)
		old := "  zizmor:\n    runs-on: ubuntu-26.04\n    timeout-minutes: 10\n    steps:\n"
		if !strings.Contains(string(wfs["ci.yml"]), old) {
			t.Fatalf("ci.yml has no zizmor job as expected")
		}
		wfs["ci.yml"] = []byte(strings.Replace(string(wfs["ci.yml"]), old, old+"      - id: docs-only\n        run: 'true'\n", 1))
		problems, err := check(wfs)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "job zizmor has a docs-only step") }) {
			t.Errorf("problems = %q, want one for zizmor", problems)
		}
	})
}
