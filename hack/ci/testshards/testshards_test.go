package testshards

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// ciWorkflow is ci.yml, from this package.
var ciWorkflow = filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml")

// pass is a test pass that ci.yml splits over the legs of a matrix job, and the required check that stands for it.
type pass struct {
	legs   string   // the matrix job
	target string   // the make target each leg runs
	family string   // each leg's cache key starts with family, the leg and "-"
	check  string   // the job named as the required check
	needs  []string // the jobs the check needs: the legs and anything else the pass's old job ran
}

var passes = []pass{
	{legs: "test-shard", target: "test-race", family: "go-test-", check: "test", needs: []string{"test-shard", "test-scripts"}},
	{legs: "test-testhooks-shard", target: "test-testhooks", family: "go-test-testhooks-", check: "test-testhooks", needs: []string{"test-testhooks-shard"}},
}

type step struct {
	ID   string         `json:"id"`
	Uses string         `json:"uses"`
	Run  string         `json:"run"`
	Env  map[string]any `json:"env"`
	With map[string]any `json:"with"`
}

type job struct {
	Name        string         `json:"name"`
	Needs       []string       `json:"needs"`
	If          string         `json:"if"`
	Permissions map[string]any `json:"permissions"`
	Strategy    struct {
		FailFast *bool          `json:"fail-fast"`
		Matrix   map[string]any `json:"matrix"`
	} `json:"strategy"`
	Steps []step `json:"steps"`
}

type workflow struct {
	Jobs map[string]job `json:"jobs"`
}

// The step that runs a leg's part of its pass: the leg's number and the number of legs come from the strategy context.
const (
	shardArg   = ` TEST_SHARD="$((LEG_INDEX + 1))/${LEG_COUNT}"`
	legIndex   = "${{ strategy.job-index }}"
	legCount   = "${{ strategy.job-total }}"
	needsJSON  = "${{ toJSON(needs) }}"
	alwaysRuns = "${{ always() }}"
)

func readWorkflow(t *testing.T) workflow {
	t.Helper()
	b, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("%s: %v", ciWorkflow, err)
	}
	return wf
}

// legNumbers returns the matrix's leg values, which must be its only key and the numbers 1 to N in order.
func legNumbers(t *testing.T, name string, j job) []int {
	t.Helper()
	if len(j.Strategy.Matrix) != 1 {
		t.Fatalf("job %s: the matrix has keys %v, want only leg", name, j.Strategy.Matrix)
	}
	values, ok := j.Strategy.Matrix["leg"].([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("job %s: matrix.leg is %v, want a list of leg numbers", name, j.Strategy.Matrix["leg"])
	}
	var legs []int
	for i, v := range values {
		if n, ok := v.(float64); !ok || n != float64(i+1) {
			t.Fatalf("job %s: matrix.leg is %v, want 1 to %d in order: the cache keys and cache-prune name legs by number", name, values, len(values))
		}
		legs = append(legs, i+1)
	}
	return legs
}

// TestLegsSplitEveryPackage: each pass's legs run the make target with their part of it, taken from the strategy
// context, keep a cache each, and are pruned each, and none stops the others when it fails.
func TestLegsSplitEveryPackage(t *testing.T) {
	wf := readWorkflow(t)
	prune, ok := wf.Jobs["cache-prune"]
	if !ok {
		t.Fatal("ci.yml has no cache-prune job")
	}
	pruneRun := ""
	for _, s := range prune.Steps {
		if strings.Contains(s.Run, "actions-cache.sh prune") {
			pruneRun = s.Run
		}
	}
	for _, p := range passes {
		t.Run(p.legs, func(t *testing.T) {
			j, ok := wf.Jobs[p.legs]
			if !ok {
				t.Fatalf("ci.yml has no job %s", p.legs)
			}
			legs := legNumbers(t, p.legs, j)
			if want := p.legs + " (${{ matrix.leg }})"; j.Name != want {
				t.Errorf("name is %q, want %q", j.Name, want)
			}
			if j.Strategy.FailFast == nil || *j.Strategy.FailFast {
				t.Error("strategy.fail-fast is not false: a failing leg would cancel the others' results")
			}
			var runs []step
			for _, s := range j.Steps {
				if strings.Contains(s.Run, "make "+p.target) {
					runs = append(runs, s)
				}
			}
			if len(runs) != 1 {
				t.Fatalf("%d steps run make %s, want one", len(runs), p.target)
			}
			if want := "make " + p.target + shardArg; strings.TrimSpace(runs[0].Run) != want {
				t.Errorf("the step runs %q, want %q", runs[0].Run, want)
			}
			if runs[0].Env["LEG_INDEX"] != legIndex || runs[0].Env["LEG_COUNT"] != legCount {
				t.Errorf("the step's LEG_INDEX and LEG_COUNT are %v and %v, want %s and %s",
					runs[0].Env["LEG_INDEX"], runs[0].Env["LEG_COUNT"], legIndex, legCount)
			}
			prefix := p.family + "${{ matrix.leg }}-${{ runner.os }}-"
			var restores int
			for _, s := range j.Steps {
				if !strings.HasPrefix(s.Uses, "actions/cache/restore@") {
					continue
				}
				restores++
				if key, _ := s.With["key"].(string); !strings.HasPrefix(key, prefix) {
					t.Errorf("the cache key %q does not start with %q", key, prefix)
				}
				if keys, _ := s.With["restore-keys"].(string); keys != prefix {
					t.Errorf("restore-keys is %q, want %q", keys, prefix)
				}
			}
			if restores != 1 {
				t.Errorf("%d cache restores, want one", restores)
			}
			if !slices.Contains(prune.Needs, p.legs) {
				t.Errorf("cache-prune needs %v, not %s, whose legs save", prune.Needs, p.legs)
			}
			for _, leg := range legs {
				if want := fmt.Sprintf(`"%s%d-${RUNNER_OS}-"`, p.family, leg); !strings.Contains(pruneRun, want) {
					t.Errorf("cache-prune does not prune %s: leg %d's entries would pile up", want, leg)
				}
			}
		})
	}
}

// TestRequiredChecksNeedEveryLeg: the jobs named as the required checks need the pass's jobs, run however those ended,
// check nothing out and hold no token permission.
func TestRequiredChecksNeedEveryLeg(t *testing.T) {
	wf := readWorkflow(t)
	for _, p := range passes {
		t.Run(p.check, func(t *testing.T) {
			j, ok := wf.Jobs[p.check]
			if !ok {
				t.Fatalf("ci.yml has no job %s", p.check)
			}
			if !slices.Equal(slices.Sorted(slices.Values(j.Needs)), slices.Sorted(slices.Values(p.needs))) {
				t.Errorf("needs %v, want %v", j.Needs, p.needs)
			}
			if j.If != alwaysRuns {
				t.Errorf("if is %q, want %q: a check whose job is skipped counts as passed", j.If, alwaysRuns)
			}
			if j.Permissions == nil {
				t.Error("permissions are not set: the job would get the workflow's, want {}")
			} else if len(j.Permissions) != 0 {
				t.Errorf("permissions are %v, want {}", j.Permissions)
			}
			if j.Name != "" {
				t.Errorf("name is %q: the check is named after the job, %s", j.Name, p.check)
			}
			if len(j.Steps) != 1 || j.Steps[0].Uses != "" || j.Steps[0].Env["NEEDS"] != needsJSON {
				t.Errorf("steps are %+v, want one run step with NEEDS set to %s and no action", j.Steps, needsJSON)
			}
		})
	}
}

// TestRequiredChecksFailUnlessEveryNeedPassed runs each required check's step, as Actions runs it (bash -e), on needs
// contexts: only one where every job it needs succeeded passes.
func TestRequiredChecksFailUnlessEveryNeedPassed(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq is not installed: the required checks' step needs it")
		}
		t.Skip("jq is not installed")
	}
	wf := readWorkflow(t)
	for _, p := range passes {
		run := wf.Jobs[p.check].Steps[0].Run
		// needsWith is the needs context in which every job passed but job, whose entry is entry.
		needsWith := func(job string, entry any) string {
			needs := map[string]any{}
			for _, n := range p.needs {
				needs[n] = map[string]any{"result": "success", "outputs": map[string]any{}}
			}
			if job != "" {
				needs[job] = entry
			}
			b, err := json.Marshal(needs)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		type tc struct {
			name, needs string
			pass        bool
		}
		cases := []tc{
			{"every job passed", needsWith("", nil), true},
			{"no job", "{}", false},
			{"an empty context", "", false},
			{"not JSON", "success", false},
		}
		for _, n := range p.needs {
			for _, r := range []string{"failure", "cancelled", "skipped", ""} {
				cases = append(cases, tc{fmt.Sprintf("%s %q", n, r), needsWith(n, map[string]any{"result": r, "outputs": map[string]any{}}), false})
			}
			cases = append(cases, tc{n + " with no result", needsWith(n, map[string]any{"outputs": map[string]any{}}), false})
		}
		for _, c := range cases {
			t.Run(p.check+"/"+c.name, func(t *testing.T) {
				cmd := exec.CommandContext(t.Context(), "bash", "--noprofile", "--norc", "-e", "-c", run)
				cmd.Env = append(os.Environ(), "NEEDS="+c.needs)
				var out bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &out
				err := cmd.Run()
				if c.pass && err != nil {
					t.Errorf("failed: %v\n%s", err, out.String())
				}
				if !c.pass && err == nil {
					t.Errorf("passed with NEEDS=%s\n%s", c.needs, out.String())
				}
			})
		}
	}
}
