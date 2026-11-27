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
	ID    string            `json:"id"`
	Env   map[string]string `json:"env"`
	Run   string            `json:"run"`
	Shell string            `json:"shell"`
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
	Env  map[string]string `json:"env"`
	Jobs map[string]job    `json:"jobs"`
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
func check(t testing.TB, list []string, workflows map[string][]byte) ([]string, error) {
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
	return append(problems, checkRelease(t, list, release)...), nil
}

// The outputs of release.yml's build job: Agent Kourier's image, the agents', addons and the chart. addons is a JSON
// object of each add-on's name to its image's reference, in hack/addons.txt's order, which sign, verify and publish go
// over; an add-on has no output of its own.
var releaseOutputs = []string{"image", "prompt-agent", "echo-agent", "addons", "chart"}

// The tag checkRelease runs release.yml's steps for, and the token they are given.
const (
	releaseTag = "v9.9.9"
	fakeToken  = "fake-token-not-a-secret"
)

// The fakes of the commands that release.yml's sign, verify and publish run besides jq. cosign logs its arguments to
// FAKE_LOG. gh answers the Releases API: the release of the tag is FAKE_RELEASE (a 404 when there is no such file), the
// generated notes are one line, and a created release's payload is kept in FAKE_PAYLOAD.
const (
	fakeCosign = `#!/bin/sh
[ "$1" = login ] && cat >/dev/null
printf '%s\n' "$*" >>"${FAKE_LOG}"
`
	fakeGH = `#!/bin/sh
case "$*" in
"api repos/"*"/releases/tags/"*)
  [ -f "${FAKE_RELEASE}" ] || { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
  cat "${FAKE_RELEASE}" ;;
"api --method POST repos/"*"/releases/generate-notes "*)
  printf '%s\n' '{"body":"## Changes\n\n* a change"}' ;;
"api --method POST repos/"*"/releases --input "*)
  for last; do :; done
  cp "${last}" "${FAKE_PAYLOAD}" ;;
*)
  echo "fake gh: unexpected arguments: $*" >&2
  exit 1 ;;
esac
`
)

// noAddonRefs are values of the build's addons output that hold no add-on's image reference by digest, which sign,
// verify and publish must stop on: none at all, an empty object, a value with a line before the reference (which a read
// loop would take as two references) and one with a newline after it.
var noAddonRefs = []string{"", "{}", `{"x":"a\nb@` + sha256Zero + `"}`, `{"x":"b@` + sha256Zero + `\n"}`}

// sha256Zero is a digest of the form release.sh reports.
var sha256Zero = "sha256:" + strings.Repeat("0", 64)

// expression is a workflow expression, ${{ ... }}.
var expression = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

// evaluate replaces each expression in value with its value in ctx, and fails on one that ctx lacks.
func evaluate(value string, ctx map[string]string) (string, error) {
	var missing []string
	out := expression.ReplaceAllStringFunc(value, func(e string) string {
		name := expression.FindStringSubmatch(e)[1]
		v, ok := ctx[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%q: no value for %q", value, missing)
	}
	return out, nil
}

// stepWithRun returns the first step of j that runs a script and, when id is not empty, has that ID.
func stepWithRun(j job, id string) step {
	for _, s := range j.Steps {
		if s.Run != "" && (id == "" || s.ID == id) {
			return s
		}
	}
	return step{}
}

// writeFile writes a file for a test, or fails it.
func writeFile(t testing.TB, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// readIfAny returns a file's content, or "" when there is no such file.
func readIfAny(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// stepResult is what a step of release.yml did: its output and error, the outputs and summary it wrote, what the fake
// cosign logged and the payload of the release the fake gh created.
type stepResult struct {
	out, summary, log, payload string
	outputs                    map[string]string
	err                        error
}

// releaseRun runs steps of release.yml as a runner does, with bash, in a checkout of its own: hack is the repository's,
// ADDONS_FILE (list) holds the add-ons it is run for, bin/release has a digest for each image release.sh reports, and
// PATH starts with the fakes.
type releaseRun struct {
	t        testing.TB
	wf       workflow
	checkout string
	fakes    string
	list     string
	// digest is each image's digest, by name: "" is Agent Kourier's, then the chart, the agents and the add-ons.
	digest map[string]string
}

// newReleaseRun returns a releaseRun of the workflow wf for the add-ons addons.
func newReleaseRun(t testing.TB, wf workflow, addons []string) *releaseRun {
	t.Helper()
	r := &releaseRun{t: t, wf: wf, checkout: t.TempDir(), fakes: t.TempDir(), digest: map[string]string{}}
	hack, err := filepath.Abs(filepath.Join(repoRoot, "hack"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hack, filepath.Join(r.checkout, "hack")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(r.checkout, "bin", "release")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range slices.Concat([]string{"", "chart", "prompt-agent", "echo-agent"}, addons) {
		file := "digest"
		if name != "" {
			file = name + "-digest"
		}
		r.digest[name] = fmt.Sprintf("sha256:%064x", i+1)
		writeFile(t, filepath.Join(out, file), r.digest[name]+"\n", 0o644)
	}
	r.list = filepath.Join(r.fakes, "addons.txt")
	writeFile(t, r.list, strings.Join(addons, "\n")+"\n", 0o644)
	writeFile(t, filepath.Join(r.fakes, "cosign"), fakeCosign, 0o755)
	writeFile(t, filepath.Join(r.fakes, "gh"), fakeGH, 0o755)
	return r
}

// run runs the step s of the job jobName with the build's outputs (nil for the build's own step) and, when release is
// not nil, that GitHub Release of the tag already published. The environment is the workflow's, the job's and the
// step's, their expressions evaluated against the build's outputs and fixed values of the github context.
func (r *releaseRun) run(jobName string, s step, outputs map[string]string, release []byte) stepResult {
	t := r.t
	t.Helper()
	tmp := t.TempDir()
	ctx := map[string]string{
		"github.ref_name": releaseTag, "github.repository": "agentkourier/agent-kourier", "github.sha": strings.Repeat("a", 40),
		"github.actor": "release-bot", "github.token": fakeToken, "secrets.GITHUB_TOKEN": fakeToken,
	}
	for k, v := range r.wf.Env {
		ctx["env."+k] = v
	}
	for k, v := range outputs {
		ctx["needs.build.outputs."+k] = v
	}
	var env []string
	for _, m := range []map[string]string{r.wf.Env, r.wf.Jobs[jobName].Env, s.Env} {
		for k, v := range m {
			value, err := evaluate(v, ctx)
			if err != nil {
				return stepResult{err: fmt.Errorf("%s: env %s: %w", jobName, k, err)}
			}
			env = append(env, k+"="+value)
		}
	}
	files := map[string]string{}
	for _, f := range []string{"GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY", "FAKE_LOG", "FAKE_PAYLOAD", "FAKE_RELEASE"} {
		files[f] = filepath.Join(tmp, strings.ToLower(f))
		env = append(env, f+"="+files[f])
	}
	if release != nil {
		writeFile(t, files["FAKE_RELEASE"], string(release), 0o644)
	}
	env = append(env, "PATH="+r.fakes+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+tmp, "TMPDIR="+tmp,
		"RUNNER_TEMP="+tmp, "ADDONS_FILE="+r.list)
	script := filepath.Join(tmp, "step.sh")
	writeFile(t, script, s.Run, 0o644)
	// A step without a shell runs as bash -e {0}; one with shell: bash as bash --noprofile --norc -eo pipefail {0}.
	args := []string{"-e", script}
	switch s.Shell {
	case "":
	case "bash":
		args = []string{"--noprofile", "--norc", "-eo", "pipefail", script}
	default:
		return stepResult{err: fmt.Errorf("%s: shell %q, which the test does not run", jobName, s.Shell)}
	}
	cmd := exec.CommandContext(t.Context(), "bash", args...)
	cmd.Dir = r.checkout
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	res := stepResult{
		out: string(out), err: err, outputs: map[string]string{}, summary: readIfAny(files["GITHUB_STEP_SUMMARY"]),
		log: readIfAny(files["FAKE_LOG"]), payload: readIfAny(files["FAKE_PAYLOAD"]),
	}
	for line := range strings.Lines(readIfAny(files["GITHUB_OUTPUT"])) {
		if k, v, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
			res.outputs[k] = v
		}
	}
	return res
}

// withOutput returns outputs with name set to value.
func withOutput(outputs map[string]string, name, value string) map[string]string {
	out := maps.Clone(outputs)
	out[name] = value
	return out
}

// orderedObject decodes a JSON object of strings into its pairs of name and value, in the object's order.
func orderedObject(s string) ([][2]string, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("not a JSON object: %q", s)
	}
	var out [][2]string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, [2]string{fmt.Sprint(tok), v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// probeList returns the add-ons checkRelease runs release.yml for: one more than list's, then list's in reverse order.
// A workflow that names the add-ons itself, rather than reading hack/addons.txt through hack/addons.sh, or that does
// not keep the list's order, then fails.
func probeList(list []string) []string {
	extra := "probe"
	for i := 2; slices.Contains(list, extra); i++ {
		extra = fmt.Sprintf("probe-%d", i)
	}
	probe := slices.Clone(list)
	slices.Reverse(probe)
	return append([]string{extra}, probe...)
}

// addonLabel is the label of an add-on's line in the release notes and the build's summary: its name upper-cased, as
// hack/security/select-release-images.sh selects it.
func addonLabel(name string) string { return strings.ToUpper(name) + " add-on image" }

// checkRelease returns where release.yml does not carry the add-ons of hack/addons.txt from its build to its release.
// It runs the build's digests step, sign, verify and publish with fakes of cosign and gh, for the add-ons of
// probeList(list). The build must report addons, each add-on's name to its image's reference in the list's order, and
// a line of the summary for each. sign and verify must go over every reference the build reports, and stop when addons
// holds none. publish must write each add-on's line in the notes, in the list's order and in the shape that
// hack/security/select-release-images.sh selects; accept, run again, the release it created and refuse one whose notes
// lack an add-on's line; and stop when addons holds none.
func checkRelease(t testing.TB, list []string, release workflow) []string {
	t.Helper()
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	build := release.Jobs["build"]
	if got, want := slices.Sorted(maps.Keys(build.Outputs)), slices.Sorted(slices.Values(releaseOutputs)); !slices.Equal(got, want) {
		add("release.yml: build's outputs are %q, want %q: an add-on's image is in addons, not an output of its own", got, want)
	}
	addons := probeList(list)
	r := newReleaseRun(t, release, addons)
	res := r.run("build", stepWithRun(build, "digests"), nil, nil)
	if res.err != nil {
		add("release.yml: build's step digests fails for the add-ons %q: %v\n%s", addons, res.err, res.out)
		return problems
	}
	steps := map[string]string{}
	for k, v := range res.outputs {
		steps["steps.digests.outputs."+k] = v
	}
	outputs := map[string]string{}
	for name, v := range build.Outputs {
		value, err := evaluate(v, steps)
		if err != nil {
			add("release.yml: build's output %s: %v", name, err)
		}
		outputs[name] = value
	}
	var want [][2]string
	for _, a := range addons {
		want = append(want, [2]string{a, release.Env["IMAGE"] + "-" + a + "@" + r.digest[a]})
	}
	if got, err := orderedObject(outputs["addons"]); err != nil || !slices.Equal(got, want) {
		add("release.yml: build's output addons is %q, want each add-on of the list (here %q) to its image's reference, in "+
			"the list's order: the digests step reads the list with hack/addons.sh", outputs["addons"], addons)
	}
	summary := strings.Split(res.summary, "\n")
	for _, p := range want {
		if line := addonLabel(p[0]) + ": `" + p[1] + "` (linux/amd64, linux/arm64)"; !slices.Contains(summary, line) {
			add("release.yml: build's step summary has no line %q", line)
		}
	}

	// sign and verify go over every reference the build reports, the add-ons' among them.
	var refs []string
	for _, name := range releaseOutputs {
		if name != "addons" {
			refs = append(refs, outputs[name])
		}
	}
	for _, p := range want {
		refs = append(refs, p[1])
	}
	for _, c := range []struct{ job, command string }{{"sign", "sign --yes "}, {"verify", "verify "}} {
		s := stepWithRun(release.Jobs[c.job], "")
		res := r.run(c.job, s, outputs, nil)
		if res.err != nil {
			add("release.yml: %s fails: %v\n%s", c.job, res.err, res.out)
			continue
		}
		logged := strings.Split(res.log, "\n")
		for _, ref := range refs {
			if !slices.ContainsFunc(logged, func(l string) bool { return strings.HasPrefix(l+" ", c.command+ref+" ") }) {
				add("release.yml: %s does not run cosign %s%s", c.job, c.command, ref)
			}
		}
		for _, none := range noAddonRefs {
			if res := r.run(c.job, s, withOutput(outputs, "addons", none), nil); res.err == nil || strings.Contains(res.log, c.command) {
				add("release.yml: %s goes on when addons is %q, which holds no add-on's image or one not a reference by digest", c.job, none)
			}
		}
	}
	return append(problems, checkPublish(r, release, outputs, want)...)
}

// checkPublish returns where release.yml's publish job, run with the build's outputs, does not write the line of each
// add-on of want (its name and reference, in the list's order) in the notes of the release it creates, or does not
// check that line in a release that exists.
func checkPublish(r *releaseRun, release workflow, outputs map[string]string, want [][2]string) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	s := stepWithRun(release.Jobs["publish"], "")
	res := r.run("publish", s, outputs, nil)
	if res.err != nil || res.payload == "" {
		return []string{fmt.Sprintf("release.yml: publish creates no release: %v\n%s", res.err, res.out)}
	}
	payload := res.payload
	var created map[string]any
	if err := json.Unmarshal([]byte(payload), &created); err != nil {
		return []string{fmt.Sprintf("release.yml: publish posts a payload that is not JSON: %v", err)}
	}
	body, _ := created["body"].(string)
	notes := strings.Split(body, "\n")
	var lines []string
	last := -1
	for _, p := range want {
		line := "- " + addonLabel(p[0]) + ": `" + p[1] + "`"
		lines = append(lines, line)
		switch i := slices.Index(notes, line); {
		case i < 0:
			add("release.yml: publish's notes have no line %q", line)
		case i < last:
			add("release.yml: publish's notes have %q before the add-on that the list has before it", line)
		default:
			last = i
		}
	}

	// The released-image rescan selects each add-on's image from those notes.
	selected, err := selectReleaseImages(r.t, body)
	if err != nil {
		add("%v", err)
	}
	for _, p := range want {
		if !slices.Contains(selected, p[1]) {
			add("hack/security/select-release-images.sh does not select %s from the notes that publish writes", p[1])
		}
	}

	// Run again, publish accepts the release it created and creates none; it refuses one whose notes lack an add-on.
	if res := r.run("publish", s, outputs, []byte(payload)); res.err != nil || res.payload != "" {
		add("release.yml: publish does not accept the release it created: %v\n%s", res.err, res.out)
	}
	for _, line := range lines {
		lacking := maps.Clone(created)
		lacking["body"] = strings.Join(slices.DeleteFunc(slices.Clone(notes), func(l string) bool { return l == line }), "\n")
		b, err := json.Marshal(lacking)
		if err != nil {
			add("%v", err)
			continue
		}
		if res := r.run("publish", s, outputs, b); res.err == nil {
			add("release.yml: publish accepts an existing release whose notes lack %q", line)
		}
	}
	for _, none := range noAddonRefs {
		if res := r.run("publish", s, withOutput(outputs, "addons", none), nil); res.err == nil || res.payload != "" {
			add("release.yml: publish goes on when addons is %q, which holds no add-on's image or one not a reference by digest", none)
		}
	}
	return problems
}

// selectReleaseImages returns the references that hack/security/select-release-images.sh selects from a final
// release of releaseTag whose notes are body.
func selectReleaseImages(t testing.TB, body string) ([]string, error) {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal([]map[string]any{{"tag_name": releaseTag, "draft": false, "prerelease": false, "body": body}})
	if err != nil {
		return nil, err
	}
	releases, out := filepath.Join(dir, "releases.json"), filepath.Join(dir, "selected")
	writeFile(t, releases, string(b), 0o644)
	cmd := exec.CommandContext(t.Context(), filepath.Join(repoRoot, "hack", "security", "select-release-images.sh"), releases, out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("hack/security/select-release-images.sh on the notes that publish writes: %w: %s", err, msg)
	}
	return strings.Fields(readIfAny(out)), nil
}

// checkProblems runs check and fails the test on an error.
func checkProblems(t *testing.T, list []string, workflows map[string][]byte) []string {
	t.Helper()
	problems, err := check(t, list, workflows)
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
	// The legs as ci.yml lists them, which the reordering case below moves (it skips with fewer than two); it holds
	// whatever the list says.
	var ci workflow
	if err := yaml.Unmarshal(repo["ci.yml"], &ci); err != nil {
		t.Fatal(err)
	}
	var legs []string
	for _, v := range ci.Jobs[addonJob].Strategy.Matrix["addon"].([]any) {
		legs = append(legs, fmt.Sprint(v))
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
		// release.yml reads the list, so the line is all it needs.
		if i := slices.IndexFunc(problems, func(p string) bool { return strings.Contains(p, "release.yml") }); i >= 0 {
			t.Errorf("problems = %q, want none of release.yml, which reads the list", problems[i])
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
	t.Run("the legs in another order", func(t *testing.T) {
		if len(legs) < 2 {
			t.Skipf("ci.yml's job %s has %d leg, so there is no other order", addonJob, len(legs))
		}
		wfs := maps.Clone(repo)
		old := "addon: [" + strings.Join(legs, ", ") + "]"
		if !strings.Contains(string(wfs["ci.yml"]), old) {
			t.Fatalf("ci.yml has no %q to change", old)
		}
		wfs["ci.yml"] = []byte(strings.Replace(string(wfs["ci.yml"]), old, "addon: ["+strings.Join(slices.Concat(legs[1:], legs[:1]), ", ")+"]", 1))
		if problems := checkProblems(t, list, wfs); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "in its order") }) {
			t.Errorf("problems = %q, want one with %q", problems, "in its order")
		}
	})
	for _, tt := range []struct {
		name, file, old, new, want, after string
	}{
		{"a leg named by the matrix's index", "ci.yml", "name: ${{ matrix.addon }}\n", "name: addon (${{ strategy.job-index }})\n", "so that each leg's check is its add-on's name", ""},
		{"a failing leg cancelling the others", "ci.yml", "      fail-fast: false\n      matrix:\n        addon:", "      fail-fast: true\n      matrix:\n        addon:", "strategy.fail-fast is not false", ""},
		{"a leg's cache not pruned", "ci.yml", `"go-` + first + `-${RUNNER_OS}-"`, `"go-other-${RUNNER_OS}-"`, `does not prune "go-` + first + `-`, ""},
		{"cache-prune not waiting for the legs", "ci.yml", "kit, addon, echo-agent", "kit, echo-agent", "does not need addon", ""},
		{"a govulncheck row of another go.sum", "security.yml", "go-sum: addons/" + first + "/go.sum", "go-sum: go.sum", "govulncheck's row", ""},
		{"a trivy row of another platform", "security.yml", "- name: " + first + "-linux-arm64\n            platform: linux/arm64", "- name: " + first + "-linux-arm64\n            platform: linux/amd64", "trivy's row", ""},
		{"a govulncheck cache not pruned", "security.yml", `"go-govulncheck-` + first + `-${RUNNER_OS}-"`, `"go-govulncheck-other-${RUNNER_OS}-"`, `does not prune "go-govulncheck-` + first + `-`, ""},
		// release.yml: after, when set, is where the change starts, for text that more than one job has.
		{"the add-ons named in the workflow", "release.yml", "list=$(hack/addons.sh)", `list=$(printf '%s\n' ` + strings.Join(list, " ") + ")", "build's output addons is", ""},
		{"the add-ons out of the list's order", "release.yml", `'. + {($name): $ref}'`, `'{($name): $ref} + .'`, "build's output addons is", ""},
		{"the add-ons not reported", "release.yml", `echo "addons=${addons}"`, `echo "addons={}"`, "build's output addons is", ""},
		{"an output of an add-on", "release.yml", "      chart: ${{ steps.digests.outputs.chart }}\n", "      third: ${{ steps.digests.outputs.third }}\n      chart: ${{ steps.digests.outputs.chart }}\n", "build's outputs are", ""},
		{"the add-ons not in the summary", "release.yml", "(linux/amd64, linux/arm64)\\n\"'", "\"'", "build's step summary has no line", ""},
		{"the add-ons' images not signed", "release.yml", `refs+=("${ref}")`, `:`, "sign does not run cosign sign --yes", "  sign:"},
		{"the add-ons' images not verified", "release.yml", `refs+=("${ref}")`, `:`, "verify does not run cosign verify", "  verify:"},
		{"no ADDON_REFS in sign's env", "release.yml", "          ADDON_REFS: ${{ needs.build.outputs.addons }}\n", "", "release.yml: sign fails", "  sign:"},
		{"the add-ons not in the notes", "release.yml", `"${ECHO_AGENT_REF}" "${addon_lines}")`, `"${ECHO_AGENT_REF}" "")`, "publish's notes have no line", "  publish:"},
		{"the add-ons not checked in an existing release", "release.yml", `and all($addon_lines | split("\n")[]; . as $line | $body | contains($line))`, "and true", "publish accepts an existing release whose notes lack", "  publish:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
