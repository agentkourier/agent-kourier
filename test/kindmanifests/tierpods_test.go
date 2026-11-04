package kindmanifests_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/labels"
)

// The namespace agent-kourier can hold several releases of the chart, and every one's pods carry app.kubernetes.io/name=agent-kourier, the
// label the front door's NetworkPolicy trusts. So what acts on a tier's own Agent Kourier has to select it by more than that label: a wait
// on the name alone times out on a release that never exits, a count finds too many pods, and a test watches the wrong pods.

const tierPods = "app.kubernetes.io/name=agent-kourier,app.kubernetes.io/instance=agent-kourier"

var waitForDelete = regexp.MustCompile(`^--context kind-agent-kourier-full -n agent-kourier wait --for=delete pod -l (\S+) --timeout=90s$`)

// runWaitTierGone runs lib.sh's wait_tier_agentkourier_gone against a kubectl stand-in with the body stub, as up.sh does (set -e), and returns
// the selectors it was asked to wait on.
func runWaitTierGone(t *testing.T, stub string) ([]string, error) {
	t.Helper()
	bin, logs := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\necho \"$*\" >> " + logs + "/argv\n" + stub + "\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eEuo pipefail; . "$1"; K=(kubectl --context kind-agent-kourier-full); wait_tier_agentkourier_gone`, "bash", "../../hack/kind/lib.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runErr := cmd.Run()
	var selectors []string
	for line := range strings.Lines(readLog(t, logs, "argv")) {
		m := waitForDelete.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			t.Fatalf("kubectl was asked %q; want a wait for pods to be deleted, in Agent Kourier's namespace on the tier's context", line)
		}
		selectors = append(selectors, m[1])
	}
	return selectors, runErr
}

// TestWaitForTheTiersAgentKourierSelectsOnlyItsOwnPods: the pods the wait holds on are the tier's, chart-made or hand-made. The selectors go
// through the Kubernetes parser, so "!key" and "key=value" mean here what they mean to the API server. The token refresh pod has its own name
// label and may sit Completed for a day; the other releases do not exit.
func TestWaitForTheTiersAgentKourierSelectsOnlyItsOwnPods(t *testing.T) {
	selectors, err := runWaitTierGone(t, "exit 0")
	if err != nil {
		t.Fatal(err)
	}
	if len(selectors) == 0 {
		t.Fatal("wait_tier_agentkourier_gone waited on nothing")
	}
	waitedOn := func(pod labels.Set) bool {
		for _, s := range selectors {
			sel, err := labels.Parse(s)
			if err != nil {
				t.Fatalf("selector %q: %v", s, err)
			}
			if sel.Matches(pod) {
				return true
			}
		}
		return false
	}
	const name, instance = "app.kubernetes.io/name", "app.kubernetes.io/instance"
	for desc, tc := range map[string]struct {
		pod  labels.Set
		want bool
	}{
		"the tier's, from the chart":              {labels.Set{name: "agent-kourier", instance: "agent-kourier"}, true},
		"the tier's, hand-made before the chart":  {labels.Set{name: "agent-kourier"}, true},
		"the sandbox's":                           {labels.Set{name: "agent-kourier", instance: "agent-kourier-sandbox"}, false},
		"another release's":                       {labels.Set{name: "agent-kourier", instance: "agent-kourier-demo-checkout"}, false},
		"a hand-made sandbox's, before the chart": {labels.Set{name: "agent-kourier", instance: "sandbox"}, false},
		"a token refresh of the release's name":   {labels.Set{name: "agent-kourier-token-refresh", instance: "agent-kourier"}, false},
		"another application's":                   {labels.Set{name: "kagent-frontdoor"}, false},
	} {
		if got := waitedOn(tc.pod); got != tc.want {
			t.Errorf("%s (%v): waited on %t, want %t; selectors %q", desc, tc.pod, got, tc.want, selectors)
		}
	}
}

// TestWaitForTheTiersAgentKourierFailsWhenAPodStays: a pod of the tier's that does not go stops the script at its first wait.
func TestWaitForTheTiersAgentKourierFailsWhenAPodStays(t *testing.T) {
	selectors, err := runWaitTierGone(t, "echo 'error: timed out waiting for the condition' >&2; exit 1")
	if err == nil || len(selectors) != 1 {
		t.Errorf("err %v after %d waits; want a failure after the first", err, len(selectors))
	}
}

// TestNothingSelectsTheTiersPodsByTheNameLabelAlone: the kind scripts and the E2E tests all find the tier's Agent Kourier by the two labels
// its Deployment selects on. A pod that a script makes may still wear the name label alone (--labels=, a probe's), which is not a selector.
func TestNothingSelectsTheTiersPodsByTheNameLabelAlone(t *testing.T) {
	nameAlone := regexp.MustCompile(`-l\s+["']?app\.kubernetes\.io/name=agent-kourier(["']?\s|["']?$)`)
	err := filepath.WalkDir("../../hack/kind", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".sh" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range nameAlone.FindAll(raw, -1) {
			t.Errorf("%s selects pods by the name label alone: %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lib), "\nTIER_AGENTKOURIER_PODS="+tierPods+"\n") {
		t.Errorf("lib.sh does not set TIER_AGENTKOURIER_PODS=%s", tierPods)
	}
	leader, err := os.ReadFile("../e2e/leader.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`agentKourierPods\s+= "` + regexp.QuoteMeta(tierPods) + `"`).Match(leader) {
		t.Errorf("test/e2e/leader.go's agentKourierPods is not %s", tierPods)
	}
	e2eGo, err := filepath.Glob("../e2e/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range e2eGo {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"app.kubernetes.io/name=agent-kourier"`) {
			t.Errorf("%s selects pods by the name label alone", f)
		}
	}
}
