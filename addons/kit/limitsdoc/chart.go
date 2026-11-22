package limitsdoc

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// startupProbeRE finds a one-line startupProbe map of a template and its fields.
var startupProbeRE = regexp.MustCompile(`(?m)^\s*startupProbe:\s*\{(.*)\}\s*$`)

// StartupProbe returns the startup probe that the chart gives the add-on's container: how many times the kubelet tries
// it (failureThreshold), and how far apart (periodSeconds), which bound the add-on's start-up work. The values are the
// chart's, not a Go package's, so a test of the add-on reads them with this and holds them to the pages with Count,
// Duration or Text. It reads charts/agent-kourier/templates/addons/<addon>/deployment.yaml, where the probe is one
// startupProbe map written on one line, and fails the test if the template has no such map, more than one, or a map
// without either field.
func StartupProbe(t testing.TB, addon string) (failures int, period time.Duration) {
	t.Helper()
	top, err := repoRoot()
	if err != nil {
		t.Fatalf("find the repository root: %v", err)
		return 0, 0
	}
	rel := path.Join("charts", "agent-kourier", "templates", "addons", addon, "deployment.yaml")
	deployment, err := os.ReadFile(filepath.Join(top, filepath.FromSlash(rel))) //nolint:gosec // a fixed path under the repository
	if err != nil {
		t.Fatalf("read the add-on's Deployment template: %v", err)
		return 0, 0
	}
	probes := startupProbeRE.FindAllSubmatch(deployment, -1)
	if len(probes) != 1 {
		t.Fatalf("%s has %d one-line startupProbe maps, and this test reads exactly one", rel, len(probes))
		return 0, 0
	}
	field := func(name string) (int, bool) {
		m := regexp.MustCompile(`\b` + name + `:\s*([0-9]+)\b`).FindSubmatch(probes[0][1])
		if m == nil {
			t.Fatalf("the add-on's startupProbe in the chart has no %s, which this test reads", name)
			return 0, false
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatalf("the add-on's startupProbe in the chart has %s %s: %v", name, m[1], err)
			return 0, false
		}
		return n, true
	}
	failures, ok := field("failureThreshold")
	if !ok {
		return 0, 0
	}
	seconds, ok := field("periodSeconds")
	if !ok {
		return 0, 0
	}
	return failures, time.Duration(seconds) * time.Second
}
