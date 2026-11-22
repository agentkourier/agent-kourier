package limitsdoc

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartupProbe reads an add-on's startup probe from the chart's template of its Deployment, a one-line map, and fails
// for a template it cannot read so: none, two maps, a map without a field, or no template at all.
func TestStartupProbe(t *testing.T) {
	cases := []struct {
		name, template string
		failures       int
		period         time.Duration
		want           string // empty: it reads the probe
	}{
		{"one map", "spec:\n  startupProbe: {httpGet: {path: /readyz, port: http}, failureThreshold: 30, periodSeconds: 2}\n", 30, 2 * time.Second, ""},
		{"no map", "spec:\n  readinessProbe: {failureThreshold: 3}\n", 0, 0, "has 0 one-line startupProbe maps"},
		{"two maps", "a:\n  startupProbe: {failureThreshold: 30, periodSeconds: 2}\nb:\n  startupProbe: {failureThreshold: 3, periodSeconds: 1}\n", 0, 0, "has 2 one-line startupProbe maps"},
		{"no period", "  startupProbe: {failureThreshold: 30}\n", 0, 0, "the add-on's startupProbe in the chart has no periodSeconds"},
		{"no template", "", 0, 0, "deployment.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := withSelftestPages(t)
			if c.template != "" {
				mustWrite(t, filepath.Join(dir, "charts", "agent-kourier", "templates", "addons", "x", "deployment.yaml"), c.template)
			}
			r := &recorder{TB: t}
			failures, period := StartupProbe(r, "x")
			joined := strings.Join(r.errs, "\n")
			switch {
			case c.want == "" && (len(r.errs) > 0 || failures != c.failures || period != c.period):
				t.Fatalf("StartupProbe = %d, %s, reported %q; want %d, %s", failures, period, joined, c.failures, c.period)
			case c.want != "" && (!r.fatal || !strings.Contains(joined, c.want)):
				t.Fatalf("reported %q (fatal %v), want a fatal error that contains %q", joined, r.fatal, c.want)
			}
		})
	}
}
