package docsonly

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
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

// gated names, per workflow, the jobs that have the docs-only step, and the class each passes to hack/ci/docs-only.sh:
// the paths that the job does not read, which docs-only.sh's header justifies for each class. A job is added here on
// purpose: gitleaks and lint, which runs zizmor's audit of the workflows, must always run in full (TestUngatedJobsRunInFull).
// ci.yml's test and test-testhooks are not gated: they check out nothing and only require their legs, which are, to
// have passed.
var gated = map[string]map[string]string{
	"ci.yml": {
		"ax":                   "ax",
		"cma":                  "cma",
		"crd-corpus":           "crd-corpus",
		"docs-agent":           "docs-agent",
		"echo-agent":           "echo-agent",
		"prompt-agent":         "prompt-agent",
		"test-postgres":        "tests",
		"test-scripts":         "tests",
		"test-shard":           "tests",
		"test-testhooks-shard": "tests",
	},
	"codeql.yml":   {"analyze": "scan"},
	"security.yml": {"govulncheck": "scan", "image": "scan"},
}

// checkoutAction is what the first step of a gated job uses, up to the pin. The pin is Renovate's to move, so the check
// holds the gated jobs' pins to each other, not to a value.
const checkoutAction = "actions/checkout@"

// The checkout's inputs, and the docs-only step after it, whose DOCS_ONLY_CLASS is the job's class in gated (classToken
// stands for it here). Neither holds anything Renovate updates.
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
  DOCS_ONLY_CLASS: ` + classToken + `
run: |
  scope=$(hack/ci/docs-only.sh "${DOCS_ONLY_CLASS}")
  echo "${scope}" >>"${GITHUB_OUTPUT}"
`
	classToken = "@class@"
)

// gate is the condition that skips a step on a docs-only pull request, and pushOnly one that holds only on a push.
const (
	gate     = "steps.docs-only.outputs.skip != 'true'"
	pushOnly = "github.event_name == 'push'"
)

var (
	// outcomeOf is a condition that a step's outcome was success; if that step runs only on a push, so does this one.
	outcomeOf = regexp.MustCompile(`^steps\.([A-Za-z0-9_-]+)\.outcome == 'success'$`)
	// statusCall is a status check function, the one call a condition may make.
	statusCall = regexp.MustCompile(`\b(?:always|cancelled|success|failure)\(\)`)
	// unreadable is what makes a condition, its status calls removed, more than a conjunction: an or, a bracket (a group
	// or a call, whose && the split would cut), or a negated event.
	unreadable = regexp.MustCompile(`\|\||[()]|!\s*github\.event_name`)
)

type step map[string]any

type workflow struct {
	Concurrency map[string]any `json:"concurrency"`
	Jobs        map[string]struct {
		If    any    `json:"if"`
		Steps []step `json:"steps"`
	} `json:"jobs"`
}

// classify reads a step's if as a conjunction: the step is gated when one of its terms is the gate, and runs only on
// a push when one is the push event or the success of a step in pushIDs. A condition with ||, a bracket other than a
// status call's, or a negated event is not read, and is a problem: a term found in it would not decide the step.
func classify(cond string, pushIDs map[string]bool) (isGated, isPushOnly bool, problem string) {
	cond = strings.TrimSpace(cond)
	if inner, ok := strings.CutPrefix(cond, "${{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok {
			cond = inner
		}
	}
	if unreadable.MatchString(statusCall.ReplaceAllString(cond, "")) {
		return false, false, fmt.Sprintf("its if %q is not a conjunction (it has ||, a bracket or a negated event)", cond)
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
	var with map[string]any
	if err := yaml.Unmarshal([]byte(wantCheckoutWith), &with); err != nil {
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
			class, want := gated[file][name]
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
			// The class first, by name, so that a job given another job's class says so; then the whole step, the
			// class put in, so that nothing else may differ (another variable, a run that ignores the class).
			if env, _ := steps[1]["env"].(map[string]any); env["DOCS_ONLY_CLASS"] != class {
				problems = append(problems, fmt.Sprintf("%s: its class (DOCS_ONLY_CLASS) is %v, not %s", where, env["DOCS_ONLY_CLASS"], class))
			}
			var docsOnly map[string]any
			if err := yaml.Unmarshal([]byte(strings.Replace(wantDocsOnly, classToken, class, 1)), &docsOnly); err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(map[string]any(steps[1]), docsOnly) {
				problems = append(problems, where+": the docs-only step is not the one every gated job has, with the job's class")
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

// testhooksRun is the make step's command in test-testhooks-shard.
const testhooksRun = "        run: make test-testhooks TEST_SHARD=\"$((LEG_INDEX + 1))/${LEG_COUNT}\"\n"

// testhooksStep is test-testhooks-shard's gated make step, whose if the drift cases below replace.
const testhooksStep = "      - if: ${{ steps.docs-only.outputs.skip != 'true' }}\n" + testhooksRun

// trimThenSave is the trim and the if of the cache save in test-shard, the first gated job of ci.yml that saves a
// cache (lint's trim has an argument, so this text is not lint's).
const trimThenSave = "        run: hack/ci/actions-cache.sh trim\n      - if: ${{ !cancelled() && steps.go-trim.outcome == 'success' }}\n"

// withTesthooksIf is testhooksStep with the if cond.
func withTesthooksIf(cond string) string {
	return "      - if: " + cond + "\n" + testhooksRun
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
		{"a step without an if", testhooksStep, "      - run: make test-testhooks TEST_SHARD=\"$((LEG_INDEX + 1))/${LEG_COUNT}\"\n", ungated},
		{"a step that always runs", testhooksStep, withTesthooksIf("${{ always() }}"), ungated},
		{"a negated gate", testhooksStep, withTesthooksIf("${{ !steps.docs-only.outputs.skip != 'true' }}"), ungated},
		{"a gate inside a longer term", testhooksStep, withTesthooksIf("${{ steps.docs-only.outputs.skip != 'true' == false }}"), ungated},
		{"the outcome of a step that runs on a pull request", testhooksStep, withTesthooksIf("${{ steps.go-cache.outcome == 'success' }}"), ungated},
		{"a push clause or'd with a pull request", testhooksStep, withTesthooksIf("${{ github.event_name == 'push' || github.event_name == 'pull_request' }}"), notConjunction},
		{"a negated push clause in a group", testhooksStep, withTesthooksIf("${{ !(github.event_name == 'push') }}"), notConjunction},
		{"a negated push clause", testhooksStep, withTesthooksIf("${{ !github.event_name == 'push' }}"), notConjunction},
		{"the gate or'd with always", testhooksStep, withTesthooksIf("${{ always() || steps.docs-only.outputs.skip != 'true' }}"), notConjunction},
		{"a push clause in a compared group", testhooksStep, withTesthooksIf("${{ (true && github.event_name == 'push' && true) == false }}"), notConjunction},
		{"the gate in a compared group", testhooksStep, withTesthooksIf("${{ (true && steps.docs-only.outputs.skip != 'true' && true) == false }}"), notConjunction},
		{"the gate as a call's argument", testhooksStep, withTesthooksIf("${{ contains(format('{0}', steps.docs-only.outputs.skip != 'true' && 'x'), 'y') }}"), notConjunction},
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
		old := "  lint:\n    runs-on: ubuntu-26.04\n    timeout-minutes: 15\n    steps:\n"
		if !strings.Contains(string(wfs["ci.yml"]), old) {
			t.Fatalf("ci.yml has no lint job as expected")
		}
		wfs["ci.yml"] = []byte(strings.Replace(string(wfs["ci.yml"]), old, old+"      - id: docs-only\n        run: 'true'\n", 1))
		if problems := checkProblems(t, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "job lint has a docs-only step") }) {
			t.Errorf("problems = %q, want one for lint", problems)
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

// inJob replaces the first old with replacement after the line that starts job name in a workflow's YAML, or fails the test.
func inJob(t *testing.T, b []byte, name, old, replacement string) []byte {
	t.Helper()
	header := "\n  " + name + ":\n"
	at := strings.Index(string(b), header)
	if at < 0 {
		t.Fatalf("no job %s", name)
	}
	rest := string(b[at:])
	if !strings.Contains(rest, old) {
		t.Fatalf("job %s (or a job after it) has no %q to change", name, old)
	}
	return []byte(string(b[:at]) + strings.Replace(rest, old, replacement, 1))
}

// TestCheckHoldsEachJobToItsClass: a gated job that passes another job's class, no class, or its class some other way
// is drift. A scanner given the tests' class would only run more, but a test leg given a scanner's would skip its
// tests on a page the tests read: both are reported.
func TestCheckHoldsEachJobToItsClass(t *testing.T) {
	repo := readWorkflows(t)
	const run = `hack/ci/docs-only.sh "${DOCS_ONLY_CLASS}"`
	for _, tt := range []struct {
		name, file, job, old, new, want string
	}{
		{"trivy given the test legs' class", "security.yml", "image", "DOCS_ONLY_CLASS: scan\n", "DOCS_ONLY_CLASS: tests\n", "job image: its class (DOCS_ONLY_CLASS) is tests, not scan"},
		{"a test leg given a scanner's class", "ci.yml", "test-testhooks-shard", "DOCS_ONLY_CLASS: tests\n", "DOCS_ONLY_CLASS: scan\n", "job test-testhooks-shard: its class (DOCS_ONLY_CLASS) is scan, not tests"},
		{"test-postgres given crd-corpus's class", "ci.yml", "test-postgres", "DOCS_ONLY_CLASS: tests\n", "DOCS_ONLY_CLASS: crd-corpus\n", "job test-postgres: its class (DOCS_ONLY_CLASS) is crd-corpus, not tests"},
		{"docs-agent given echo-agent's class", "ci.yml", "docs-agent", "DOCS_ONLY_CLASS: docs-agent\n", "DOCS_ONLY_CLASS: echo-agent\n", "job docs-agent: its class (DOCS_ONLY_CLASS) is echo-agent, not docs-agent"},
		{"cma given the docs agent's class", "ci.yml", "cma", "DOCS_ONLY_CLASS: cma\n", "DOCS_ONLY_CLASS: docs-agent\n", "job cma: its class (DOCS_ONLY_CLASS) is docs-agent, not cma"},
		{"ax given the CMA add-on's class", "ci.yml", "ax", "DOCS_ONLY_CLASS: ax\n", "DOCS_ONLY_CLASS: cma\n", "job ax: its class (DOCS_ONLY_CLASS) is cma, not ax"},
		{"govulncheck given the docs agent's class", "security.yml", "govulncheck", "DOCS_ONLY_CLASS: scan\n", "DOCS_ONLY_CLASS: docs-agent\n", "job govulncheck: its class (DOCS_ONLY_CLASS) is docs-agent, not scan"},
		{"no class", "ci.yml", "crd-corpus", "          DOCS_ONLY_CLASS: crd-corpus\n", "", "job crd-corpus: its class (DOCS_ONLY_CLASS) is <nil>, not crd-corpus"},
		{"the class from an expression", "codeql.yml", "analyze", "DOCS_ONLY_CLASS: scan\n", "DOCS_ONLY_CLASS: ${{ matrix.class }}\n", "job analyze: its class (DOCS_ONLY_CLASS) is ${{ matrix.class }}, not scan"},
		{"a run that names a class itself", "ci.yml", "echo-agent", run, "hack/ci/docs-only.sh scan", "job echo-agent: the docs-only step is not the one"},
		{"a run that drops the class", "ci.yml", "test-shard", run, "hack/ci/docs-only.sh", "job test-shard: the docs-only step is not the one"},
		{"the class beside another variable", "security.yml", "image", "DOCS_ONLY_CLASS: scan\n", "DOCS_ONLY_CLASS: scan\n          JOB_CLASS: tests\n", "job image: the docs-only step is not the one"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := maps.Clone(repo)
			wfs[tt.file] = inJob(t, wfs[tt.file], tt.job, tt.old, tt.new)
			if problems := checkProblems(t, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("problems = %q, want one with %q", problems, tt.want)
			}
		})
	}
}

// TestEveryClassIsTheScripts: hack/ci/docs-only.sh accepts each class a gated job passes. The script runs everything on
// a class it does not know, so a class missing from it would cost the job its skip without failing anything.
func TestEveryClassIsTheScripts(t *testing.T) {
	classes := map[string]bool{}
	for _, jobs := range gated {
		for _, class := range jobs {
			classes[class] = true
		}
	}
	script, err := filepath.Abs("../docs-only.sh")
	if err != nil {
		t.Fatal(err)
	}
	// stderr runs the script on class, with no PR_HEAD_SHA, which it checks right after the class, from a directory that
	// is no repository, and returns what it printed there.
	stderr := func(class string) string {
		cmd := exec.CommandContext(t.Context(), script, class)
		cmd.Dir = t.TempDir()
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "PR_HEAD_SHA=") }), "PR_HEAD_SHA=")
		var out strings.Builder
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s %s: %v (%s)", script, class, err, out.String())
		}
		return out.String()
	}
	if got := stderr("no-such-class"); !strings.Contains(got, "is not a class") {
		t.Fatalf("an unknown class: stderr %q, want it refused as not a class (control)", got)
	}
	for _, class := range slices.Sorted(maps.Keys(classes)) {
		if got := stderr(class); !strings.Contains(got, "PR_HEAD_SHA is not set") {
			t.Errorf("class %s: stderr %q, want the script past the class, at PR_HEAD_SHA", class, got)
		}
	}
}

// ungated names, per workflow, the jobs (by id) that run in full on every pull request: lint, and security.yml's
// secrets, whose check is named gitleaks. check reports a docs-only step in any of them; this list holds them to
// existing, so a rename cannot leave the rule covering nothing.
var ungated = map[string][]string{
	"ci.yml":       {"lint"},
	"security.yml": {"secrets"},
}

// The step of ci.yml's lint that audits the workflows with zizmor, the one if it may have, and the one env: it runs
// whatever the steps before it did, on every event, so a lint or build failure does not hide a finding; and without
// the token hack/security/lint-workflows.sh runs zizmor offline, which drops its online audits (impostor commits,
// known vulnerable actions) without failing.
const (
	auditRun   = "make lint-workflows"
	auditIf    = "${{ !cancelled() }}"
	auditToken = "${{ github.token }}"
)

// ungatedProblems returns what is wrong with the jobs in ungated: one that is missing, a lint with a job-level if (it
// must run on every event), and a lint without the workflow audit as wanted.
func ungatedProblems(t *testing.T, workflows map[string][]byte) []string {
	t.Helper()
	var problems []string
	for _, file := range slices.Sorted(maps.Keys(ungated)) {
		var wf workflow
		if err := yaml.Unmarshal(workflows[file], &wf); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, name := range ungated[file] {
			if _, ok := wf.Jobs[name]; !ok {
				problems = append(problems, fmt.Sprintf("%s has no job %s", file, name))
			}
		}
		if file != "ci.yml" {
			continue
		}
		if cond := wf.Jobs["lint"].If; cond != nil {
			problems = append(problems, fmt.Sprintf("ci.yml: job lint has the if %v; it must run on every event", cond))
		}
		var audits []step
		for _, s := range wf.Jobs["lint"].Steps {
			if s["run"] == auditRun {
				audits = append(audits, s)
			}
		}
		switch {
		case len(audits) != 1:
			problems = append(problems, fmt.Sprintf("ci.yml: job lint has %d steps that run %s, not one", len(audits), auditRun))
		case audits[0]["if"] != auditIf:
			problems = append(problems, fmt.Sprintf("ci.yml: job lint: the %s step's if is %v, not %s", auditRun, audits[0]["if"], auditIf))
		case !reflect.DeepEqual(audits[0]["env"], map[string]any{"GH_TOKEN": auditToken}):
			problems = append(problems, fmt.Sprintf("ci.yml: job lint: the %s step's env is %v, not GH_TOKEN: %s alone", auditRun, audits[0]["env"], auditToken))
		}
	}
	return problems
}

// TestUngatedJobsRunInFull: lint and secrets exist, and lint audits the workflows on every run.
func TestUngatedJobsRunInFull(t *testing.T) {
	for _, p := range ungatedProblems(t, readWorkflows(t)) {
		t.Error(p)
	}
}

// TestUngatedProblemsFindDrift: each way the workflow audit can leave lint, or lint leave ci.yml, is reported.
func TestUngatedProblemsFindDrift(t *testing.T) {
	repo := readWorkflows(t)
	const (
		run  = "        run: " + auditRun + "\n        env:\n          GH_TOKEN: ${{ github.token }}\n"
		step = "      - if: " + auditIf + "\n" + run
	)
	for _, tt := range []struct {
		name, file, old, new, want string
	}{
		{"the audit dropped", "ci.yml", step, "", "has 0 steps that run make lint-workflows"},
		{"the audit twice", "ci.yml", step, step + step, "has 2 steps that run make lint-workflows"},
		{"the audit only on a push", "ci.yml", step, "      - if: ${{ github.event_name == 'push' }}\n" + run, "the make lint-workflows step's if is ${{ github.event_name == 'push' }}"},
		{"the audit skipped after a failure", "ci.yml", step, "      - " + strings.TrimPrefix(run, "        "), "the make lint-workflows step's if is <nil>"},
		{"the token dropped", "ci.yml", step, "      - if: " + auditIf + "\n        run: " + auditRun + "\n", "the make lint-workflows step's env is <nil>"},
		{"another variable beside the token", "ci.yml", step, step + "          ZIZMOR_BIN: /tmp/zizmor\n", "the make lint-workflows step's env is"},
		{"lint only on a push", "ci.yml", "\n  lint:\n    runs-on:", "\n  lint:\n    if: ${{ github.event_name == 'push' }}\n    runs-on:", "job lint has the if ${{ github.event_name == 'push' }}"},
		{"lint renamed", "ci.yml", "\n  lint:\n", "\n  lint-and-build:\n", "ci.yml has no job lint"},
		{"secrets renamed", "security.yml", "\n  secrets:\n", "\n  gitleaks-scan:\n", "security.yml has no job secrets"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := maps.Clone(repo)
			if !strings.Contains(string(wfs[tt.file]), tt.old) {
				t.Fatalf("%s has no %q to change", tt.file, tt.old)
			}
			wfs[tt.file] = []byte(strings.Replace(string(wfs[tt.file]), tt.old, tt.new, 1))
			if problems := ungatedProblems(t, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("problems = %q, want one with %q", problems, tt.want)
			}
		})
	}
}

// codeqlConcurrency is codeql.yml's concurrency, whose comment there says why: a pull request's newer push cancels its
// run, and every other run is a group of its own, keyed by its run id. A group that main's pushes shared (the tidier
// ${{ github.workflow }}-${{ github.ref }}) would let a newer push cancel a pending one, whose commit the release gate
// (hack/release/check-ci.sh) then refuses, its codeql run cancelled.
var codeqlConcurrency = map[string]any{
	"group":              "${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.ref || github.run_id }}",
	"cancel-in-progress": "${{ github.event_name == 'pull_request' }}",
}

// concurrencyProblems returns what is wrong with codeql.yml's concurrency: anything but codeqlConcurrency.
func concurrencyProblems(t *testing.T, workflows map[string][]byte) []string {
	t.Helper()
	var wf workflow
	if err := yaml.Unmarshal(workflows["codeql.yml"], &wf); err != nil {
		t.Fatalf("codeql.yml: %v", err)
	}
	if !reflect.DeepEqual(wf.Concurrency, codeqlConcurrency) {
		return []string{fmt.Sprintf("codeql.yml: its concurrency is %v, not %v", wf.Concurrency, codeqlConcurrency)}
	}
	return nil
}

// TestCodeQLConcurrencyCancelsOnlyPullRequests: codeql.yml's concurrency is the one wanted.
func TestCodeQLConcurrencyCancelsOnlyPullRequests(t *testing.T) {
	for _, p := range concurrencyProblems(t, readWorkflows(t)) {
		t.Error(p)
	}
}

// TestConcurrencyProblemsFindDrift: a group that main's pushes share, a cancel on every event, or no concurrency at
// all is reported.
func TestConcurrencyProblemsFindDrift(t *testing.T) {
	repo := readWorkflows(t)
	const (
		group  = "  group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.ref || github.run_id }}\n"
		cancel = "  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n"
	)
	for _, tt := range []struct{ name, old, new string }{
		{"one group per ref", group, "  group: ${{ github.workflow }}-${{ github.ref }}\n"},
		{"cancel on every event", cancel, "  cancel-in-progress: true\n"},
		{"no concurrency", "concurrency:\n" + group + cancel, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := maps.Clone(repo)
			if !strings.Contains(string(wfs["codeql.yml"]), tt.old) {
				t.Fatalf("codeql.yml has no %q to change", tt.old)
			}
			wfs["codeql.yml"] = []byte(strings.Replace(string(wfs["codeql.yml"]), tt.old, tt.new, 1))
			if problems := concurrencyProblems(t, wfs); len(problems) == 0 {
				t.Error("no problem reported")
			}
		})
	}
}
