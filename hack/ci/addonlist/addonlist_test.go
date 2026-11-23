package addonlist

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// repoRoot is the repository, from this package.
var repoRoot = filepath.Join("..", "..", "..")

type step struct {
	Run string `json:"run"`
}

type job struct {
	Name     string   `json:"name"`
	Needs    []string `json:"needs"`
	Strategy struct {
		FailFast *bool          `json:"fail-fast"`
		Matrix   map[string]any `json:"matrix"`
	} `json:"strategy"`
	Steps []step `json:"steps"`
}

type workflow struct {
	Jobs map[string]job `json:"jobs"`
}

// The add-on job of ci.yml, and what its legs are named: each leg's check is its add-on's name.
const (
	addonJob  = "addon"
	addonName = "${{ matrix.addon }}"
)

// The platforms security.yml scans each add-on's image for, as release.sh builds it.
var platforms = []string{"linux/amd64", "linux/arm64"}

// readList returns the add-ons that hack/addons.sh reads from hack/addons.txt: the reader the Makefile and the release
// scripts use, so the test reads the list as they do.
func readList(t *testing.T) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(repoRoot, "hack", "addons.sh"))
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "ADDONS_FILE=") })
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hack/addons.sh: %v (%s)", err, stderr.String())
	}
	return strings.Fields(string(out))
}

// readWorkflows returns ci.yml and security.yml.
func readWorkflows(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, file := range []string{"ci.yml", "security.yml"} {
		b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		out[file] = b
	}
	return out
}

// pruneRun returns the run of the step of job cache-prune that prunes the caches.
func pruneRun(wf workflow) string {
	for _, s := range wf.Jobs["cache-prune"].Steps {
		if strings.Contains(s.Run, "actions-cache.sh prune") {
			return s.Run
		}
	}
	return ""
}

// include returns the include rows of a job's matrix, each value as a string.
func include(j job) []map[string]string {
	rows, _ := j.Strategy.Matrix["include"].([]any)
	var out []map[string]string
	for _, r := range rows {
		m, _ := r.(map[string]any)
		row := map[string]string{}
		for k, v := range m {
			row[k] = fmt.Sprint(v)
		}
		out = append(out, row)
	}
	return out
}

// check returns where the workflows (file name to YAML) do not hold the add-ons in list: ci.yml's add-on job, a leg
// per add-on in the list's order, named after it, none cancelling the others, each cache pruned; and security.yml's
// govulncheck of each add-on's module and trivy scan of each add-on's image on each platform, each cache pruned, with
// no row for an add-on the list lacks.
func check(list []string, workflows map[string][]byte) ([]string, error) {
	var ci, security workflow
	if err := yaml.Unmarshal(workflows["ci.yml"], &ci); err != nil {
		return nil, fmt.Errorf("ci.yml: %w", err)
	}
	if err := yaml.Unmarshal(workflows["security.yml"], &security); err != nil {
		return nil, fmt.Errorf("security.yml: %w", err)
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	listed := func(name string) bool { return slices.Contains(list, name) }

	// ci.yml: the add-on job.
	j, ok := ci.Jobs[addonJob]
	if !ok {
		add("ci.yml has no job %s", addonJob)
	} else {
		var legs []string
		values, _ := j.Strategy.Matrix["addon"].([]any)
		for _, v := range values {
			legs = append(legs, fmt.Sprint(v))
		}
		if !slices.Equal(legs, list) {
			add("ci.yml: job %s: matrix.addon is %q, want hack/addons.txt's %q, in its order", addonJob, legs, list)
		}
		if len(j.Strategy.Matrix) != 1 {
			add("ci.yml: job %s: the matrix has keys besides addon: %v", addonJob, j.Strategy.Matrix)
		}
		if j.Name != addonName {
			add("ci.yml: job %s: name is %q, want %q, so that each leg's check is its add-on's name", addonJob, j.Name, addonName)
		}
		if j.Strategy.FailFast == nil || *j.Strategy.FailFast {
			add("ci.yml: job %s: strategy.fail-fast is not false: a failing leg would cancel the other add-ons' results", addonJob)
		}
	}
	if !slices.Contains(ci.Jobs["cache-prune"].Needs, addonJob) {
		add("ci.yml: job cache-prune does not need %s, so it can prune before the legs save", addonJob)
	}
	run := pruneRun(ci)
	for _, a := range list {
		if want := `"go-` + a + `-${RUNNER_OS}-"`; !strings.Contains(run, want) {
			add("ci.yml: cache-prune does not prune %s, the cache of the %s leg", want, a)
		}
	}

	// security.yml: one govulncheck row for each add-on's module, no other under addons/ but the kit's.
	var modules []string
	for _, row := range include(security.Jobs["govulncheck"]) {
		dir := row["directory"]
		if !strings.HasPrefix(dir, "addons/") || dir == "addons/kit" {
			continue
		}
		m := row["module"]
		modules = append(modules, m)
		want := map[string]string{"module": m, "directory": "addons/" + m, "go-sum": "addons/" + m + "/go.sum"}
		if !reflect.DeepEqual(row, want) {
			add("security.yml: govulncheck's row %v is not %v", row, want)
		}
		if !listed(m) {
			add("security.yml: govulncheck checks %s, which hack/addons.txt does not list", dir)
		}
	}
	run = pruneRun(security)
	for _, a := range list {
		if !slices.Contains(modules, a) {
			add("security.yml: govulncheck has no row for addons/%s", a)
		}
		if want := `"go-govulncheck-` + a + `-${RUNNER_OS}-"`; !strings.Contains(run, want) {
			add("security.yml: cache-prune does not prune %s, the cache of govulncheck (%s)", want, a)
		}
	}

	// security.yml: one trivy row for each add-on's image on each platform.
	scans := map[string][]string{}
	for _, row := range include(security.Jobs["image"]) {
		a, ok := row["addon"]
		if !ok {
			continue
		}
		scans[a] = append(scans[a], row["platform"])
		want := map[string]string{"name": a + "-" + strings.ReplaceAll(row["platform"], "/", "-"), "platform": row["platform"], "addon": a}
		if !reflect.DeepEqual(row, want) {
			add("security.yml: trivy's row %v is not %v", row, want)
		}
		if !listed(a) {
			add("security.yml: trivy scans the image of %s, which hack/addons.txt does not list", a)
		}
	}
	for _, a := range list {
		if got := slices.Sorted(slices.Values(scans[a])); !slices.Equal(got, platforms) {
			add("security.yml: trivy scans the image of %s for %q, want %q", a, got, platforms)
		}
	}
	return problems, nil
}

// checkProblems runs check and fails the test on an error.
func checkProblems(t *testing.T, list []string, workflows map[string][]byte) []string {
	t.Helper()
	problems, err := check(list, workflows)
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

// TestWorkflowsHoldEveryAddon: the workflows hold the add-ons of hack/addons.txt, and no other.
func TestWorkflowsHoldEveryAddon(t *testing.T) {
	list := readList(t)
	if len(list) == 0 {
		t.Fatal("hack/addons.sh lists no add-on")
	}
	for _, p := range checkProblems(t, list, readWorkflows(t)) {
		t.Error(p)
	}
}

// TestCheckFindsDrift: an add-on added to the list alone, or a workflow's row of one changed or dropped, is reported.
func TestCheckFindsDrift(t *testing.T) {
	list := readList(t)
	repo := readWorkflows(t)
	first := list[0]
	// The legs as ci.yml lists them, which the reordering case below moves; it holds whatever the list says.
	var ci workflow
	if err := yaml.Unmarshal(repo["ci.yml"], &ci); err != nil {
		t.Fatal(err)
	}
	var legs []string
	for _, v := range ci.Jobs[addonJob].Strategy.Matrix["addon"].([]any) {
		legs = append(legs, fmt.Sprint(v))
	}
	if len(legs) < 2 {
		t.Fatalf("ci.yml's job %s has %d legs; the reordering case needs two", addonJob, len(legs))
	}
	t.Run("an add-on in the list alone", func(t *testing.T) {
		problems := checkProblems(t, append(slices.Clone(list), "third"), repo)
		for _, want := range []string{
			"matrix.addon is", `cache-prune does not prune "go-third-${RUNNER_OS}-"`, "govulncheck has no row for addons/third",
			`cache-prune does not prune "go-govulncheck-third-${RUNNER_OS}-"`, "trivy scans the image of third for []",
		} {
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
				t.Errorf("problems = %q, want one with %q", problems, want)
			}
		}
	})
	t.Run("an add-on dropped from the list alone", func(t *testing.T) {
		problems := checkProblems(t, list[1:], repo)
		for _, want := range []string{
			"matrix.addon is", "govulncheck checks addons/" + first + ", which hack/addons.txt does not list",
			"trivy scans the image of " + first + ", which hack/addons.txt does not list",
		} {
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
				t.Errorf("problems = %q, want one with %q", problems, want)
			}
		}
	})
	for _, tt := range []struct {
		name, file, old, new, want string
	}{
		{"the legs in another order", "ci.yml", "addon: [" + strings.Join(legs, ", ") + "]", "addon: [" + strings.Join(slices.Concat(legs[1:], legs[:1]), ", ") + "]", "in its order"},
		{"a leg named by the matrix's index", "ci.yml", "name: ${{ matrix.addon }}\n", "name: addon (${{ strategy.job-index }})\n", "so that each leg's check is its add-on's name"},
		{"a failing leg cancelling the others", "ci.yml", "      fail-fast: false\n      matrix:\n        addon:", "      fail-fast: true\n      matrix:\n        addon:", "strategy.fail-fast is not false"},
		{"a leg's cache not pruned", "ci.yml", `"go-` + first + `-${RUNNER_OS}-"`, `"go-other-${RUNNER_OS}-"`, `does not prune "go-` + first + `-`},
		{"cache-prune not waiting for the legs", "ci.yml", "kit, addon, echo-agent", "kit, echo-agent", "does not need addon"},
		{"a govulncheck row of another go.sum", "security.yml", "go-sum: addons/" + first + "/go.sum", "go-sum: go.sum", "govulncheck's row"},
		{"a trivy row of another platform", "security.yml", "- name: " + first + "-linux-arm64\n            platform: linux/arm64", "- name: " + first + "-linux-arm64\n            platform: linux/amd64", "trivy's row"},
		{"a govulncheck cache not pruned", "security.yml", `"go-govulncheck-` + first + `-${RUNNER_OS}-"`, `"go-govulncheck-other-${RUNNER_OS}-"`, `does not prune "go-govulncheck-` + first + `-`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := map[string][]byte{"ci.yml": repo["ci.yml"], "security.yml": repo["security.yml"]}
			if !strings.Contains(string(wfs[tt.file]), tt.old) {
				t.Fatalf("%s has no %q to change", tt.file, tt.old)
			}
			wfs[tt.file] = []byte(strings.Replace(string(wfs[tt.file]), tt.old, tt.new, 1))
			if problems := checkProblems(t, list, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("problems = %q, want one with %q", problems, tt.want)
			}
		})
	}
}
