package limitsdoc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	page := "intro\n| a | `4000`<!--addon-limit:cma.runes--> | b |\n\n`2 MiB`<!--addon-limit:cma.event--> and `60s`<!--addon-limit:other.keepalive-->\n" +
		"<!--limit:triggers--> `50`<!--limit:broker-->\n"
	got := parse(page)
	want := []mark{{"cma.runes", "4000", 2}, {"cma.event", "2 MiB", 4}, {"other.keepalive", "60s", 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("parse = %v, want %v", got, want)
	}
	if bad := malformed(page); len(bad) != 0 {
		t.Fatalf("malformed = %v for a page whose add-on markers all follow a code span", bad)
	}
}

func TestMalformed(t *testing.T) {
	page := strings.Join([]string{
		"`1`<!--addon-limit:cma.good-->",
		"no value <!--addon-limit:cma.bare-->",
		"`2` <!--addon-limit:cma.spaced-->",
		"`3`<!--addon-limit:cma.bad id-->",
		"`4`<!--addon-limit:-->",
		"`5`<!--limit:broker.only-->",
	}, "\n")
	if got, want := malformed(page), []int{2, 3, 4, 5}; !slices.Equal(got, want) {
		t.Fatalf("malformed = %v, want the lines %v", got, want)
	}
}

// recorder is a testing.TB that records what the guard functions report, so that their failures can be asserted.
type recorder struct {
	testing.TB
	errs  []string
	fatal bool
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.fatal = true
	r.Errorf(format, args...)
}

// TestGuardsReportADifference holds the four guards to what they say: a value that equals the page's passes, and one that
// does not names the page line, what the page says and what the code has. A limit written at two places is checked at each.
func TestGuardsReportADifference(t *testing.T) {
	cases := []struct {
		name string
		run  func(t testing.TB)
		want string // empty: it passes
	}{
		{"count equal", func(t testing.TB) { Count(t, "selftest.count", 7) }, ""},
		{"count differs", func(t testing.TB) { Count(t, "selftest.count", 8) }, "says `7` for the limit \"selftest.count\" (a plain number), the code has 8"},
		{"count not a number", func(t testing.TB) { Count(t, "selftest.text", 1) }, "says `1.2` for the limit \"selftest.text\" (a plain number)"},
		{"bytes equal", func(t testing.TB) { Bytes(t, "selftest.bytes", 2<<20) }, ""},
		{"bytes differ", func(t testing.TB) { Bytes(t, "selftest.bytes", 4<<20) }, "the code has 4194304 bytes"},
		{"kib", func(t testing.TB) { Bytes(t, "selftest.kib", 341<<10) }, ""},
		{"duration equal", func(t testing.TB) { Duration(t, "selftest.duration", 90*24*time.Hour) }, ""},
		{"duration differs", func(t testing.TB) { Duration(t, "selftest.duration", time.Hour) }, "the code has 1h0m0s"},
		{"milliseconds", func(t testing.TB) { Duration(t, "selftest.ms", 250*time.Millisecond) }, ""},
		{"a bad duration", func(t testing.TB) { Duration(t, "selftest.count", 7) }, "write a duration as a number and ms, s, m, h or d"},
		{"text equal", func(t testing.TB) { Text(t, "selftest.text", "1.2") }, ""},
		{"text differs", func(t testing.TB) { Text(t, "selftest.text", "1.3") }, "the code has `1.3`"},
		{"a bad unit", func(t testing.TB) { Bytes(t, "selftest.badunit", 1) }, "write a size as a number and bytes, KiB or MiB"},
		{"twice, both equal", func(t testing.TB) { Count(t, "selftest.twice", 5) }, ""},
		{"twice, one differs", func(t testing.TB) { Count(t, "selftest.twicediffer", 5) }, ":3 says `6` for the limit \"selftest.twicediffer\""},
		{"a limit not on the page", func(t testing.TB) { Count(t, "selftest.absent", 1) }, "has no marker <!--addon-limit:selftest.absent-->"},
		{"the broker's marker is not the add-on's", func(t testing.TB) { Count(t, "selftest.broker", 9) }, "has no marker <!--addon-limit:selftest.broker-->"},
	}
	withSelftestPage(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{TB: t}
			c.run(r)
			joined := strings.Join(r.errs, "\n")
			switch {
			case c.want == "" && len(r.errs) > 0:
				t.Fatalf("reported %q, want a pass", joined)
			case c.want != "" && !strings.Contains(joined, c.want):
				t.Fatalf("reported %q, want it to contain %q", joined, c.want)
			}
		})
	}
}

// withSelftestPage points the guards at a page of this test's own, by making a repository of a temporary directory that
// holds the page under a go.mod of an add-on module, and working in that module, as the add-on's tests do.
func withSelftestPage(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	page := strings.Join([]string{
		"| x | `7`<!--addon-limit:selftest.count--> | `2 MiB`<!--addon-limit:selftest.bytes--> `341 KiB`<!--addon-limit:selftest.kib-->",
		"`90d`<!--addon-limit:selftest.duration--> `250ms`<!--addon-limit:selftest.ms--> `1.2`<!--addon-limit:selftest.text-->",
		"`2 TiB`<!--addon-limit:selftest.badunit--> `5`<!--addon-limit:selftest.twice--> `5`<!--addon-limit:selftest.twicediffer--> `6`<!--addon-limit:selftest.twicediffer-->",
		"`5`<!--addon-limit:selftest.twice--> `9`<!--limit:selftest.broker-->",
	}, "\n")
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(PagePath)), page)
	module := filepath.Join(dir, "addons", "x")
	mustWrite(t, filepath.Join(module, "go.mod"), "module x\n")
	t.Chdir(module)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The values below are the chart's, not a Go package's: the page states them, and they are read from the chart's files.

// TestChartLimits holds the add-on's startup probe in the chart to the page: how many times the kubelet tries it, and how far
// apart, which bound the start-up work.
func TestChartLimits(t *testing.T) {
	top, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := os.ReadFile(filepath.Join(top, "charts", "agent-kourier", "templates", "addons", "cma", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	probes := regexp.MustCompile(`(?m)^\s*startupProbe:\s*\{(.*)\}\s*$`).FindAllSubmatch(deployment, -1)
	if len(probes) != 1 {
		t.Fatalf("charts/agent-kourier/templates/addons/cma/deployment.yaml has %d one-line startupProbe maps, and this test reads exactly one", len(probes))
	}
	field := func(name string) int {
		t.Helper()
		m := regexp.MustCompile(`\b` + name + `:\s*([0-9]+)\b`).FindSubmatch(probes[0][1])
		if m == nil {
			t.Fatalf("the add-on's startupProbe in the chart has no %s, which this test reads", name)
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	failures, period := field("failureThreshold"), field("periodSeconds")
	Count(t, "cma.startup-failures", failures)
	Duration(t, "cma.startup-period", time.Duration(period)*time.Second)
}
