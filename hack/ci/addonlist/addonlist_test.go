package addonlist

import (
	"encoding/json"
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

// repoRoot is the repository, from this package.
var repoRoot = filepath.Join("..", "..", "..")

type step struct {
	ID  string            `json:"id"`
	Env map[string]string `json:"env"`
	Run string            `json:"run"`
}

// needs is a job's needs, which a workflow writes as one job's ID or as a list of them.
type needs []string

// UnmarshalJSON reads a job's ID or a list of them.
func (n *needs) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*n = needs{one}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("needs: %w", err)
	}
	*n = list
	return nil
}

type job struct {
	Name     string            `json:"name"`
	Needs    needs             `json:"needs"`
	Outputs  map[string]string `json:"outputs"`
	Env      map[string]string `json:"env"`
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

// readWorkflows returns ci.yml, security.yml and release.yml.
func readWorkflows(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, file := range []string{"ci.yml", "security.yml", "release.yml"} {
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
// per add-on in the list's order, named after it, none cancelling the others, each cache pruned; security.yml's
// govulncheck of each add-on's module and trivy scan of each add-on's image on each platform, each cache pruned, with
// no row for an add-on the list lacks; and release.yml's outputs, signatures and notes (checkRelease).
func check(list []string, workflows map[string][]byte) ([]string, error) {
	var ci, security, release workflow
	for file, wf := range map[string]*workflow{"ci.yml": &ci, "security.yml": &security, "release.yml": &release} {
		if err := yaml.Unmarshal(workflows[file], wf); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
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
	return append(problems, checkRelease(list, release)...), nil
}

// The outputs of release.yml's build job that are not an add-on's: Agent Kourier's image, the agents' and the chart.
var releaseOutputs = []string{"image", "prompt-agent", "echo-agent", "chart"}

// releaseNames returns what release.yml calls an add-on: its shell and jq variable, its environment variable and the
// label of its line in the release notes. The publish job writes that line, and hack/security/select-release-images.sh
// selects it for the released-image rescan.
func releaseNames(a string) (variable, env, label string) {
	variable = strings.ReplaceAll(a, "-", "_")
	return variable, strings.ToUpper(variable) + "_REF", strings.ToUpper(a) + " add-on image"
}

// The env entries that take a build output, and the release notes' body: its format and its arguments.
var (
	buildOutputRef = regexp.MustCompile(`^\$\{\{ needs\.build\.outputs\.([a-z0-9-]+) \}\}$`)
	notesPrintf    = regexp.MustCompile(`body=\$\(printf "([^"]*)" \\\n\s*([^)]*)\)`)
	notesLine      = regexp.MustCompile("\\\\n- ([^:\\\\]+): \\\\`%s\\\\`")
	notesArg       = regexp.MustCompile(`"\$\{([A-Za-z_]+)\}"`)
)

// loopStep returns the step of a job whose run goes over the references, `for ref in ...`, and that loop's line.
func loopStep(j job) (step, string) {
	for _, s := range j.Steps {
		for line := range strings.Lines(s.Run) {
			if strings.HasPrefix(strings.TrimSpace(line), "for ref in ") {
				return s, strings.TrimSpace(line)
			}
		}
	}
	return step{}, ""
}

// checkRelease returns where release.yml does not carry each add-on of list from its build to its release: the build
// job's output and the digest it reports, the reference that sign, verify and publish each take and that the first two
// go over, the publish job's check of an existing release and the line it writes in the notes; and an output, or a
// reference taken from one, of an add-on the list lacks.
func checkRelease(list []string, release workflow) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	build := release.Jobs["build"]
	for name := range build.Outputs {
		if !slices.Contains(releaseOutputs, name) && !slices.Contains(list, name) {
			add("release.yml: build has an output %s, which hack/addons.txt does not list", name)
		}
	}
	var digests string
	for _, s := range build.Steps {
		if s.ID == "digests" {
			digests = s.Run
		}
	}
	for _, a := range list {
		v, _, label := releaseNames(a)
		if got, want := build.Outputs[a], "${{ steps.digests.outputs."+a+" }}"; got != want {
			add("release.yml: build's output %s is %q, want %q", a, got, want)
		}
		for _, want := range []string{
			v + `="${IMAGE}-` + a + `@$(cat bin/release/` + a + `-digest)"`,
			`echo "` + a + `=${` + v + `}"`,
			`echo "` + label + `: \` + "`${" + v + `}\` + "`",
		} {
			if !strings.Contains(digests, want) {
				add("release.yml: build's step digests does not have %s, which reports the digest of %s", want, a)
			}
		}
	}

	for _, name := range []string{"sign", "verify", "publish"} {
		j := release.Jobs[name]
		s, loop := loopStep(j)
		env := j.Env
		if name != "publish" {
			env = s.Env
		}
		for key, value := range env {
			if m := buildOutputRef.FindStringSubmatch(value); m != nil && !slices.Contains(releaseOutputs, m[1]) && !slices.Contains(list, m[1]) {
				add("release.yml: %s's %s takes the build output %s, which hack/addons.txt does not list", name, key, m[1])
			}
		}
		for _, a := range list {
			_, ref, _ := releaseNames(a)
			if got, want := env[ref], "${{ needs.build.outputs."+a+" }}"; got != want {
				add("release.yml: %s's %s is %q, want %q", name, ref, got, want)
			}
			if name != "publish" && !strings.Contains(loop, `"${`+ref+`}"`) {
				add("release.yml: %s's loop %q does not go over ${%s}, the image of %s", name, loop, ref, a)
			}
		}
	}

	var publish string
	if steps := release.Jobs["publish"].Steps; len(steps) > 0 {
		publish = steps[0].Run
	}
	notes := map[string]string{}
	if m := notesPrintf.FindStringSubmatch(publish); m == nil {
		add("release.yml: publish writes no release notes with printf")
	} else {
		labels := notesLine.FindAllStringSubmatch(m[1], -1)
		args := notesArg.FindAllStringSubmatch(m[2], -1)
		if len(args) != len(labels)+1 {
			add("release.yml: publish's notes have %d lines with a reference and %d references after the body", len(labels), len(args)-1)
		} else {
			for i, l := range labels {
				notes[l[1]] = args[i+1][1]
			}
		}
	}
	for _, a := range list {
		v, ref, label := releaseNames(a)
		if got := notes[label]; got != ref {
			add("release.yml: publish's notes have no line %q with ${%s} (it has %q)", "- "+label+": `...`", ref, got)
		}
		for _, want := range []string{
			`--arg ` + v + ` "${` + ref + `}"`,
			`contains("` + label + ": `\" + $" + v + ` + "` + "`" + `")`,
		} {
			if !strings.Contains(publish, want) {
				add("release.yml: publish's check of an existing release does not have %s, for %s", want, a)
			}
		}
	}
	return problems
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
	first, last := list[0], list[len(list)-1]
	lastVar, lastRef, lastLabel := releaseNames(last)
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
			"build's output third is", `bin/release/third-digest`, "sign's THIRD_REF is", "verify's loop", "publish's THIRD_REF is",
			"publish's notes have no line \"- THIRD add-on image", `--arg third "${THIRD_REF}"`,
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
	t.Run("the notes' lines in another order", func(t *testing.T) {
		if first == last {
			t.Skip("the list has one add-on, so there is no other order")
		}
		_, _, firstLabel := releaseNames(first)
		wfs := maps.Clone(repo)
		text := string(wfs["release.yml"])
		at := strings.Index(text, "body=$(printf")
		swap := strings.NewReplacer(`\n- `+firstLabel+": ", `\n- `+lastLabel+": ", `\n- `+lastLabel+": ", `\n- `+firstLabel+": ")
		wfs["release.yml"] = []byte(text[:at] + swap.Replace(text[at:]))
		if problems := checkProblems(t, list, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "publish's notes have no line") }) {
			t.Errorf("problems = %q, want the notes' lines and references paired", problems)
		}
	})
	for _, tt := range []struct {
		name, file, old, new, want, after string
	}{
		{"the legs in another order", "ci.yml", "addon: [" + strings.Join(legs, ", ") + "]", "addon: [" + strings.Join(slices.Concat(legs[1:], legs[:1]), ", ") + "]", "in its order", ""},
		{"a leg named by the matrix's index", "ci.yml", "name: ${{ matrix.addon }}\n", "name: addon (${{ strategy.job-index }})\n", "so that each leg's check is its add-on's name", ""},
		{"a failing leg cancelling the others", "ci.yml", "      fail-fast: false\n      matrix:\n        addon:", "      fail-fast: true\n      matrix:\n        addon:", "strategy.fail-fast is not false", ""},
		{"a leg's cache not pruned", "ci.yml", `"go-` + first + `-${RUNNER_OS}-"`, `"go-other-${RUNNER_OS}-"`, `does not prune "go-` + first + `-`, ""},
		{"cache-prune not waiting for the legs", "ci.yml", "kit, addon, echo-agent", "kit, echo-agent", "does not need addon", ""},
		{"a govulncheck row of another go.sum", "security.yml", "go-sum: addons/" + first + "/go.sum", "go-sum: go.sum", "govulncheck's row", ""},
		{"a trivy row of another platform", "security.yml", "- name: " + first + "-linux-arm64\n            platform: linux/arm64", "- name: " + first + "-linux-arm64\n            platform: linux/amd64", "trivy's row", ""},
		{"a govulncheck cache not pruned", "security.yml", `"go-govulncheck-` + first + `-${RUNNER_OS}-"`, `"go-govulncheck-other-${RUNNER_OS}-"`, `does not prune "go-govulncheck-` + first + `-`, ""},
		// release.yml: after, when set, is where the change starts, for text that more than one job has.
		{"an add-on's image not signed", "release.yml", `"${` + lastRef + `}" `, "", "sign's loop", "  sign:"},
		{"an add-on's image not verified", "release.yml", `"${` + lastRef + `}" `, "", "verify's loop", "  verify:"},
		{"an add-on's reference not in sign's env", "release.yml", "          " + lastRef + ": ", "          OTHER_REF: ", "sign's " + lastRef + " is", "  sign:"},
		{"an output of an unlisted add-on", "release.yml", "      chart: ${{ steps.digests.outputs.chart }}\n", "      third: ${{ steps.digests.outputs.third }}\n      chart: ${{ steps.digests.outputs.chart }}\n", "build has an output third, which hack/addons.txt does not list", ""},
		{"a reference of an unlisted add-on", "release.yml", "      CHART_REF: ", "      THIRD_REF: ${{ needs.build.outputs.third }}\n      CHART_REF: ", "publish's THIRD_REF takes the build output third", "  publish:"},
		{"an add-on's line not in the notes", "release.yml", "\\n- " + lastLabel + ": ", "\\n- Other image: ", "publish's notes have no line \"- " + lastLabel, "body=$(printf"},
		{"an add-on not checked in an existing release", "release.yml", "contains(\"" + lastLabel + ": ", "contains(\"Other image: ", "publish's check of an existing release does not have", "  publish:"},
		{"an add-on's digest not reported", "release.yml", `echo "` + last + `=${` + lastVar + `}"`, `echo "` + last + `="`, "build's step digests does not have", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wfs := maps.Clone(repo)
			text := string(wfs[tt.file])
			at := strings.Index(text, tt.after)
			if at < 0 || !strings.Contains(text[at:], tt.old) {
				t.Fatalf("%s has no %q to change after %q", tt.file, tt.old, tt.after)
			}
			wfs[tt.file] = []byte(text[:at] + strings.Replace(text[at:], tt.old, tt.new, 1))
			if problems := checkProblems(t, list, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("problems = %q, want one with %q", problems, tt.want)
			}
		})
	}
}

// cacheName finds the name in a cache's key or prune prefix, go-<name>-${{ runner.os }} or "go-<name>-${RUNNER_OS}-".
var cacheName = regexp.MustCompile(`go-([a-z0-9]+(?:-[a-z0-9]+)*)-\$\{(?:\{ runner\.os|RUNNER_OS)`)

// validName is the syntax of an add-on's name that hack/addons.sh accepts; a name the workflows use outside it cannot be
// listed in the first place.
var validName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// takenNames returns the names that the workflows (file name to YAML) give to something an add-on's name would collide
// with: job IDs, check names, govulncheck's modules, trivy's images, caches and release.yml's outputs. It leaves out the
// add-ons of list, their caches among them, and names hack/addons.sh's syntax refuses anyway.
func takenNames(t *testing.T, list []string, workflows map[string][]byte) []string {
	t.Helper()
	ours := func(n string) bool {
		return slices.Contains(list, n) || slices.ContainsFunc(list, func(a string) bool { return n == "govulncheck-"+a })
	}
	seen := map[string]bool{}
	take := func(n string) {
		if validName.MatchString(n) && !ours(n) {
			seen[n] = true
		}
	}
	for file, b := range workflows {
		var wf workflow
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for id, j := range wf.Jobs {
			take(id)
			take(j.Name)
			for k := range j.Outputs {
				take(k)
			}
			for _, row := range include(j) {
				take(row["module"])
				take(row["agent"])
				for _, p := range platforms {
					if n, ok := strings.CutSuffix(row["name"], "-"+strings.ReplaceAll(p, "/", "-")); ok {
						take(n)
					}
				}
			}
		}
		for _, m := range cacheName.FindAllStringSubmatch(string(b), -1) {
			take(m[1])
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// TestAddonsRefusesTakenNames: hack/addons.sh refuses every name the workflows already give to a job, a check, a
// cache, an image or a release output, so that an add-on listed under one cannot take its check or its cache.
func TestAddonsRefusesTakenNames(t *testing.T) {
	list := readList(t)
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflows := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		workflows[filepath.Base(f)] = b
	}
	taken := takenNames(t, list, workflows)
	for _, want := range []string{"lint", "prompt-agent", "root", "chart", "test-1", "gitleaks"} {
		if !slices.Contains(taken, want) {
			t.Fatalf("takenNames = %q, which lacks %s: the workflows were not read as the test expects", taken, want)
		}
	}
	file := filepath.Join(t.TempDir(), "addons.txt")
	if err := os.WriteFile(file, []byte(strings.Join(taken, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), filepath.Join(repoRoot, "hack", "addons.sh"))
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "ADDONS_FILE=") }), "ADDONS_FILE="+file)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("hack/addons.sh accepted the names the workflows take, printing %q", out)
	}
	for i, n := range taken {
		if want := fmt.Sprintf("line %d: %s is ", i+1, n); !strings.Contains(stderr.String(), want) {
			t.Errorf("hack/addons.sh does not refuse %s, which a workflow takes: add it to TAKEN (stderr: %s)", n, stderr.String())
		}
	}
}

// chartNamesFile is the chart's template that defines agentkourier.addon.names, the add-ons the chart renders.
var chartNamesFile = filepath.Join("charts", "agent-kourier", "templates", "addons", "_addon.tpl")

// chartNames finds the body of the define of agentkourier.addon.names.
var chartNames = regexp.MustCompile(`\{\{-?\s*define "agentkourier\.addon\.names"\s*-?\}\}(.*?)\{\{-?\s*end\s*-?\}\}`)

// checkChart returns where the chart's agentkourier.addon.names (tpl, the text of _addon.tpl) is not the add-ons of
// list, in its order: the names joined by single spaces, which the chart's templates split on one space.
func checkChart(list []string, tpl []byte) []string {
	m := chartNames.FindAllSubmatch(tpl, -1)
	if len(m) != 1 {
		return []string{fmt.Sprintf("%s defines agentkourier.addon.names %d times, want once", chartNamesFile, len(m))}
	}
	if got, want := string(m[0][1]), strings.Join(list, " "); got != want {
		return []string{fmt.Sprintf("%s: agentkourier.addon.names is %q, want hack/addons.txt's %q, in its order", chartNamesFile, got, want)}
	}
	return nil
}

// TestChartNamesEveryAddon: the chart renders the add-ons of hack/addons.txt, and no other.
func TestChartNamesEveryAddon(t *testing.T) {
	list := readList(t)
	tpl, err := os.ReadFile(filepath.Join(repoRoot, chartNamesFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range checkChart(list, tpl) {
		t.Error(p)
	}
	define := `{{- define "agentkourier.addon.names" -}}` + strings.Join(list, " ") + `{{- end -}}`
	for _, tt := range []struct {
		name, old, new, want string
	}{
		{"an add-on in the list alone", define, `{{- define "agentkourier.addon.names" -}}` + strings.Join(list[1:], " ") + `{{- end -}}`, "agentkourier.addon.names is"},
		{"an add-on in the chart alone", define, `{{- define "agentkourier.addon.names" -}}` + strings.Join(append(slices.Clone(list), "third"), " ") + `{{- end -}}`, "agentkourier.addon.names is"},
		{"two spaces between names", define, `{{- define "agentkourier.addon.names" -}}` + strings.Join(list, "  ") + `{{- end -}}`, "agentkourier.addon.names is"},
		{"no define", define, "", "0 times"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(string(tpl), tt.old) {
				t.Fatalf("%s has no %q to change", chartNamesFile, tt.old)
			}
			problems := checkChart(list, []byte(strings.Replace(string(tpl), tt.old, tt.new, 1)))
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("problems = %q, want one with %q", problems, tt.want)
			}
		})
	}
}
