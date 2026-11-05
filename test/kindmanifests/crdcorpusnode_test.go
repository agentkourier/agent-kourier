package kindmanifests_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// readRepoFile returns a file of the repository, by its path from the root.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// nodeImage is the variable of hack/kind/lib.sh named (NODE_IMAGE or NODE_IMAGE_MIN), read as a shell does, with its tag and Kubernetes minor.
func nodeImage(t *testing.T, variable string) (tag, minor string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `. ../../hack/kind/lib.sh && printf %s "${`+variable+`}"`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reading %s from hack/kind/lib.sh: %v", variable, err)
	}
	m := regexp.MustCompile(`^kindest/node:(v(1\.\d+)\.\d+)@sha256:[0-9a-f]{64}$`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("%s = %q; want kindest/node:v1.N.P@sha256:<64 hex digits>, pinned by digest", variable, out)
	}
	return m[1], m[2]
}

// TestTheCorpusMinimumIsTheChartMinimum: the CRD corpus runs on NODE_IMAGE_MIN to cover the oldest Kubernetes the chart admits, so its minor is
// the one the chart's check (crMode in _helpers.tpl) refuses everything below. A chart that moves its minimum leaves the corpus testing a version
// nobody can install, or one above what it admits, unless this fails.
func TestTheCorpusMinimumIsTheChartMinimum(t *testing.T) {
	_, minor := nodeImage(t, "NODE_IMAGE_MIN")
	m := regexp.MustCompile(`semverCompare ">=(1\.\d+)\.0-0" \.Capabilities\.KubeVersion\.Version`).FindStringSubmatch(readRepoFile(t, "charts/agent-kourier/templates/_helpers.tpl"))
	if m == nil {
		t.Fatal("charts/agent-kourier/templates/_helpers.tpl has no semverCompare of the Kubernetes version against a 1.N.0-0 minimum")
	}
	if minor != m[1] {
		t.Errorf("NODE_IMAGE_MIN in hack/kind/lib.sh is Kubernetes %s and the chart's minimum is %s: move them together, with the job names crd-corpus (1.xx) in ci.yml and check-ci.sh", minor, m[1])
	}
}

// TestRenovateKeepsTheMinimumNodeImageOnItsMinor: NODE_IMAGE_MIN and NODE_IMAGE are the same repository, so the regex manager that bumps the one
// would bump the other to the newest Kubernetes, and the corpus would stop covering the minimum without a failure. A package rule that disables
// minor and major updates for a current value on the minimum's minor holds it there; its patch and digest updates still arrive.
func TestRenovateKeepsTheMinimumNodeImageOnItsMinor(t *testing.T) {
	minTag, minor := nodeImage(t, "NODE_IMAGE_MIN")
	defaultTag, _ := nodeImage(t, "NODE_IMAGE")
	var cfg struct {
		PackageRules []struct {
			MatchDepNames     []string `json:"matchDepNames"`
			MatchCurrentValue string   `json:"matchCurrentValue"`
			MatchUpdateTypes  []string `json:"matchUpdateTypes"`
			Enabled           *bool    `json:"enabled"`
		} `json:"packageRules"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, "renovate.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, r := range cfg.PackageRules {
		if !slices.Contains(r.MatchDepNames, "kindest/node") || r.Enabled == nil || *r.Enabled || r.MatchCurrentValue == "" {
			continue
		}
		if len(r.MatchCurrentValue) < 2 || !strings.HasPrefix(r.MatchCurrentValue, "/") || !strings.HasSuffix(r.MatchCurrentValue, "/") {
			t.Fatalf("renovate.json: matchCurrentValue %q is not a /regex/ without flags", r.MatchCurrentValue)
		}
		re, err := regexp.Compile(r.MatchCurrentValue[1 : len(r.MatchCurrentValue)-1])
		if err != nil {
			t.Fatalf("renovate.json: matchCurrentValue %q: %v", r.MatchCurrentValue, err)
		}
		// The rule must hold the minimum's tag, leave NODE_IMAGE's alone, and refuse the updates that change the minor.
		if re.MatchString(minTag) && !re.MatchString(defaultTag) && slices.Contains(r.MatchUpdateTypes, "minor") && slices.Contains(r.MatchUpdateTypes, "major") {
			return
		}
	}
	t.Errorf("renovate.json has no package rule that disables minor and major updates of kindest/node for a current value on v%s (matchCurrentValue), so Renovate would move NODE_IMAGE_MIN off the chart's minimum", minor)
}

// TestTheCorpusJobsAreTheReleaseGatesJobs: the jobs ci.yml's crd-corpus matrix makes are named crd-corpus (<minor>), and each is a job
// check-ci.sh requires, so a release cannot go out on a commit whose corpus failed or never ran, on either Kubernetes. A rename in one place
// fails the gate for a reason that looks like a missing job; this fails first. The job for the chart's minimum runs NODE_IMAGE_MIN
// (CRD_CORPUS_NODE=min) and the other the pinned node.
func TestTheCorpusJobsAreTheReleaseGatesJobs(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			Name     string `json:"name"`
			Strategy struct {
				Matrix struct {
					Include []map[string]string `json:"include"`
				} `json:"matrix"`
			} `json:"strategy"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".github/workflows/ci.yml")), &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["crd-corpus"]
	if !ok {
		t.Fatal("ci.yml has no crd-corpus job")
	}
	const jobName = "crd-corpus (${{ matrix.k8s }})"
	if job.Name != jobName {
		t.Fatalf("the crd-corpus job's name is %q; want %q, which the main ruleset and check-ci.sh name once per matrix entry", job.Name, jobName)
	}
	_, minorOfDefault := nodeImage(t, "NODE_IMAGE")
	_, minorOfMin := nodeImage(t, "NODE_IMAGE_MIN")
	gate := readRepoFile(t, "hack/release/check-ci.sh")
	got := map[string]string{}
	for _, entry := range job.Strategy.Matrix.Include {
		name := fmt.Sprintf("crd-corpus (%s)", entry["k8s"])
		got[name] = entry["node"]
		if !strings.Contains(gate, "'"+name+"'") {
			t.Errorf("check-ci.sh does not require %q, a job ci.yml makes", name)
		}
	}
	want := map[string]string{
		"crd-corpus (" + minorOfDefault + ")": "default",
		"crd-corpus (" + minorOfMin + ")":     "min",
	}
	if !slices.Equal(sortedPairs(got), sortedPairs(want)) {
		t.Errorf("ci.yml's crd-corpus matrix makes (job name, CRD_CORPUS_NODE) %v; want %v: the pinned node and the chart's minimum, one job each", got, want)
	}
}

// sortedPairs lists a map's entries as "key=value", sorted, for comparing two maps.
func sortedPairs(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}
