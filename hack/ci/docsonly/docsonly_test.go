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

// checkoutAction is what the first step of a gated job uses, up to the pin. The pin is Renovate's to move, so the check
// holds the gated jobs' pins to each other, not to a value.
const checkoutAction = "actions/checkout@"

// The checkout's inputs, and the docs-only step after it. Neither holds anything Renovate updates.
const (
	wantCheckoutWith = `
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

// gate is the condition that skips a step on a docs-only pull request, and pushOnly one that holds only on a push.
const (
	gate     = "steps.docs-only.outputs.skip != 'true'"
	pushOnly = "github.event_name == 'push'"
)

var (
	// outcomeOf is a condition that a step's outcome was success; if that step runs only on a push, so does this one.
	outcomeOf = regexp.MustCompile(`^steps\.([A-Za-z0-9_-]+)\.outcome == 'success'$`)
	// unreadable is what makes a condition more than a conjunction: an or, or a negation of a group or of an event.
	unreadable = regexp.MustCompile(`\|\||!\s*\(|!\s*github\.event_name`)
)

type step map[string]any

type workflow struct {
	Jobs map[string]struct {
		Steps []step `json:"steps"`
	} `json:"jobs"`
}

// classify reads a step's if as a conjunction: the step is gated when one of its terms is the gate, and runs only on
// a push when one is the push event or the success of a step in pushIDs. A condition with || or a negated group or
// event is not read, and is a problem: a term found in it would not decide the step.
func classify(cond string, pushIDs map[string]bool) (isGated, isPushOnly bool, problem string) {
	cond = strings.TrimSpace(cond)
	if inner, ok := strings.CutPrefix(cond, "${{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok {
			cond = inner
		}
	}
	if unreadable.MatchString(cond) {
		return false, false, fmt.Sprintf("its if %q is not a conjunction (it has || or a negated group or event)", cond)
	}
	for term := range strings.SplitSeq(cond, "&&") {
		term = strings.TrimSpace(term)
		switch term {
		case gate:
			isGated = true
		case pushOnly:
			isPushOnly = true
		default:
			if m := outcomeOf.FindStringSubmatch(term); m != nil && pushIDs[m[1]] {
				isPushOnly = true
			}
		}
	}
	return isGated, isPushOnly, ""
}

// check returns what is wrong with the docs-only gate of the workflows (file name to YAML): a gated job whose first
// two steps are not the checkout and the docs-only step as wanted, gated jobs whose checkouts differ, a job not in gated
// with the docs-only step, or a later step that a pull request runs without the gate.
func check(workflows map[string][]byte) ([]string, error) {
	var with, docsOnly map[string]any
	if err := yaml.Unmarshal([]byte(wantCheckoutWith), &with); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(wantDocsOnly), &docsOnly); err != nil {
		return nil, err
	}
	var problems []string
	var checkouts []string // "<uses>\t<file>: job <name>", per gated job
	for _, file := range slices.Sorted(maps.Keys(workflows)) {
		var wf workflow
		if err := yaml.Unmarshal(workflows[file], &wf); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
			where := fmt.Sprintf("%s: job %s", file, name)
			steps := wf.Jobs[name].Steps
			at := slices.IndexFunc(steps, func(s step) bool { return s["id"] == "docs-only" })
			want := slices.Contains(gated[file], name)
			switch {
			case at < 0 && !want:
				continue
			case at < 0:
				problems = append(problems, where+" has no docs-only step")
				continue
			case !want:
				problems = append(problems, where+" has a docs-only step but is not one of the gated jobs")
				continue
			case at != 1:
				problems = append(problems, fmt.Sprintf("%s: the docs-only step is step %d, not the second", where, at+1))
				continue
			}
			uses, _ := steps[0]["uses"].(string)
			if !strings.HasPrefix(uses, checkoutAction) || len(steps[0]) != 2 || !reflect.DeepEqual(steps[0]["with"], with) {
				problems = append(problems, where+": the first step is not the checkout of a gated job (uses and with, with only these inputs)")
			}
			checkouts = append(checkouts, uses+"\t"+where)
			if !reflect.DeepEqual(map[string]any(steps[1]), docsOnly) {
				problems = append(problems, where+": the docs-only step is not the one every gated job has")
			}
			pushIDs := map[string]bool{}
			for i, s := range steps[2:] {
				cond, _ := s["if"].(string)
				isGated, isPushOnly, problem := classify(cond, pushIDs)
				switch {
				case problem != "":
					problems = append(problems, fmt.Sprintf("%s: step %d: %s", where, i+3, problem))
				case isPushOnly:
					if id, _ := s["id"].(string); id != "" {
						pushIDs[id] = true
					}
				case !isGated:
					problems = append(problems, fmt.Sprintf("%s: step %d runs on a pull request without the docs-only gate", where, i+3))
				}
			}
		}
	}
	for _, c := range checkouts {
		uses, where, _ := strings.Cut(c, "\t")
		firstUses, firstWhere, _ := strings.Cut(checkouts[0], "\t")
		if uses != firstUses {
			problems = append(problems, fmt.Sprintf("%s: its checkout is %s, not %s as in %s", where, uses, firstUses, firstWhere))
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

// checkProblems runs check and fails the test on an error.
func checkProblems(t *testing.T, workflows map[string][]byte) []string {
	t.Helper()
	problems, err := check(workflows)
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

// TestWorkflowsGateAlike: every gated job of the repository's workflows has the gate as wanted, and no other job has it.
func TestWorkflowsGateAlike(t *testing.T) {
	for _, p := range checkProblems(t, readWorkflows(t)) {
		t.Error(p)
	}
}

// testhooksStep is test-testhooks' gated make step, whose if the drift cases below replace.
const testhooksStep = "      - if: ${{ steps.docs-only.outputs.skip != 'true' }}\n        run: make test-testhooks\n"

// trimThenSave is the trim and the if of the cache save in test, the first gated job of ci.yml (lint's trim has an
// argument, so this text is not lint's).
const trimThenSave = "        run: hack/ci/actions-cache.sh trim\n      - if: ${{ !cancelled() && steps.go-trim.outcome == 'success' }}\n"

// withTesthooksIf is testhooksStep with the if cond.
func withTesthooksIf(cond string) string {
	return "      - if: " + cond + "\n        run: make test-testhooks\n"
}

// TestCheckFindsDrift: each way a copy of the gate can drift is reported.
func TestCheckFindsDrift(t *testing.T) {
	repo := readWorkflows(t)
	const notConjunction = "is not a conjunction"
	const ungated = "runs on a pull request without the docs-only gate"
	for _, tt := range []struct {
		name, old, new, want string
	}{
		{"a docs-only step without continue-on-error", "        continue-on-error: true\n", "", "the docs-only step is not the one"},
		{"a docs-only step without the head", "          PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}\n", "          PR_HEAD_SHA: ''\n", "the docs-only step is not the one"},
		{"a checkout of depth 1", "          fetch-depth: ${{ github.event_name == 'pull_request' && 2 || 1 }}\n", "", "the first step is not the checkout"},
		{"a checkout with an if", "          fetch-depth: ${{ github.event_name == 'pull_request' && 2 || 1 }}\n", "          fetch-depth: ${{ github.event_name == 'pull_request' && 2 || 1 }}\n        if: false\n", "the first step is not the checkout"},
		{"a gated job without the step", "      - id: docs-only\n", "      - id: docs-only-renamed\n", "has no docs-only step"},
		{"a step without an if", testhooksStep, "      - run: make test-testhooks\n", ungated},
		{"a step that always runs", testhooksStep, withTesthooksIf("${{ always() }}"), ungated},
		{"a negated gate", testhooksStep, withTesthooksIf("${{ !steps.docs-only.outputs.skip != 'true' }}"), ungated},
		{"a gate inside a longer term", testhooksStep, withTesthooksIf("${{ steps.docs-only.outputs.skip != 'true' == false }}"), ungated},
		{"the outcome of a step that runs on a pull request", testhooksStep, withTesthooksIf("${{ steps.go-cache.outcome == 'success' }}"), ungated},
		{"a push clause or'd with a pull request", testhooksStep, withTesthooksIf("${{ github.event_name == 'push' || github.event_name == 'pull_request' }}"), notConjunction},
		{"a negated push clause in a group", testhooksStep, withTesthooksIf("${{ !(github.event_name == 'push') }}"), notConjunction},
		{"a negated push clause", testhooksStep, withTesthooksIf("${{ !github.event_name == 'push' }}"), notConjunction},
		{"the gate or'd with always", testhooksStep, withTesthooksIf("${{ always() || steps.docs-only.outputs.skip != 'true' }}"), notConjunction},
		{"a push-only outcome or'd with always", trimThenSave, "        run: hack/ci/actions-cache.sh trim\n      - if: ${{ steps.go-trim.outcome == 'success' || always() }}\n", notConjunction},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := maps.Clone(repo)
			if !strings.Contains(string(wfs["ci.yml"]), tt.old) {
				t.Fatalf("ci.yml has no %q to change", tt.old)
			}
			wfs["ci.yml"] = []byte(strings.Replace(string(wfs["ci.yml"]), tt.old, tt.new, 1))
			if problems := checkProblems(t, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
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
		if problems := checkProblems(t, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "job zizmor has a docs-only step") }) {
			t.Errorf("problems = %q, want one for zizmor", problems)
		}
	})
}

// gatedCheckoutPin finds the pin of a gated job's checkout (the one that fetches the merge commit's parents).
var gatedCheckoutPin = regexp.MustCompile(`actions/checkout@[0-9a-f]{40}(?: # \S+)?(\n        with:\n          persist-credentials: false\n          # The merge commit)`)

// otherPin stands in for the pin Renovate would move the checkout to.
const otherPin = "actions/checkout@0123456789abcdef0123456789abcdef01234567 # v99.0.0"

// TestCheckHoldsTheCheckoutPinsAlike: one gated job's checkout pinned apart from the others is drift; every checkout
// moved to a new pin together, as Renovate moves them, is not.
func TestCheckHoldsTheCheckoutPinsAlike(t *testing.T) {
	repo := readWorkflows(t)
	t.Run("one job's pin differs", func(t *testing.T) {
		wfs := maps.Clone(repo)
		loc := gatedCheckoutPin.FindSubmatchIndex(wfs["security.yml"])
		if loc == nil {
			t.Fatal("security.yml has no gated checkout as expected")
		}
		b := wfs["security.yml"]
		wfs["security.yml"] = slices.Concat(b[:loc[0]], []byte(otherPin), b[loc[2]:])
		want := "its checkout is " + strings.Fields(otherPin)[0]
		if problems := checkProblems(t, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
			t.Errorf("problems = %q, want one for the checkout pinned apart", problems)
		}
	})
	t.Run("every pin moves together", func(t *testing.T) {
		wfs := maps.Clone(repo)
		pin := regexp.MustCompile(`actions/checkout@[0-9a-f]{40}(?: # \S+)?`)
		for f, b := range wfs {
			wfs[f] = pin.ReplaceAll(b, []byte(otherPin))
		}
		for _, p := range checkProblems(t, wfs) {
			t.Error(p)
		}
	})
}
