package limitsdoc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestParse(t *testing.T) {
	page := "intro\n| a | `50`<!--limit:triggers--> | b |\n\n`2 MiB`<!--limit:event--> and `341 KiB`<!--limit:reply-->\n"
	got := parse(page)
	want := []mark{{"triggers", "50", 2}, {"event", "2 MiB", 4}, {"reply", "341 KiB", 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("parse = %v, want %v", got, want)
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
// does not names the page line, what the page says and what the code has.
func TestGuardsReportADifference(t *testing.T) {
	cases := []struct {
		name string
		run  func(t testing.TB)
		want string // empty: it passes
	}{
		{"count equal", func(t testing.TB) { Count(t, "selftest.count", 7) }, ""},
		{"count differs", func(t testing.TB) { Count(t, "selftest.count", 8) }, "says `7` for the limit \"selftest.count\" (a plain number), the code has 8"},
		{"bytes equal", func(t testing.TB) { Bytes(t, "selftest.bytes", 2<<20) }, ""},
		{"bytes differ", func(t testing.TB) { Bytes(t, "selftest.bytes", 4<<20) }, "the code has 4194304 bytes"},
		{"kib", func(t testing.TB) { Bytes(t, "selftest.kib", 341<<10) }, ""},
		{"duration equal", func(t testing.TB) { Duration(t, "selftest.duration", 90*24*time.Hour) }, ""},
		{"duration differs", func(t testing.TB) { Duration(t, "selftest.duration", time.Hour) }, "the code has 1h0m0s"},
		{"text equal", func(t testing.TB) { Text(t, "selftest.text", "1.33") }, ""},
		{"text differs", func(t testing.TB) { Text(t, "selftest.text", "1.34") }, "the code has `1.34`"},
		{"a bad unit", func(t testing.TB) { Bytes(t, "selftest.badunit", 1) }, "write a size as a number and bytes, KiB or MiB"},
		{"a limit not on the page", func(t testing.TB) { Count(t, "selftest.absent", 1) }, "has no marker <!--limit:selftest.absent-->"},
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

// withSelftestPage points the guards at a page of this test's own, by making a repository root of a temporary directory
// that holds a go.mod and the page, and working in it.
func withSelftestPage(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	page := strings.Join([]string{
		"| x | `7`<!--limit:selftest.count--> |",
		"`2 MiB`<!--limit:selftest.bytes--> `341 KiB`<!--limit:selftest.kib--> `90d`<!--limit:selftest.duration-->",
		"`1.33`<!--limit:selftest.text--> `2 TiB`<!--limit:selftest.badunit-->",
	}, "\n")
	mustWrite(t, filepath.Join(dir, "go.mod"), "module selftest\n")
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(PagePath)), page)
	t.Chdir(dir)
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

// guardCallRE finds a call of a guard with a literal ID. In another package the call is limitsdoc.Count(t, "id", ...), and in
// this one it is Count(t, "id", ...).
var guardCallRE = regexp.MustCompile(`(limitsdoc\.)?\b(?:Count|Bytes|Duration|Text)\(\s*[A-Za-z_][A-Za-z0-9_]*\s*,\s*"([^"]+)"`)

// TestEveryMarkerHasAGuard fails for a marker of the page that no test of the repository holds to the code, and for a marker
// that is not a code span with a value before it, which the guards could not read.
func TestEveryMarkerHasAGuard(t *testing.T) {
	top, err := root()
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(filepath.Join(top, PagePath))
	if err != nil {
		t.Fatal(err)
	}
	marks := parse(string(page))
	if raw := anyMarkerRE.FindAllString(string(page), -1); len(raw) != len(marks) {
		t.Errorf("%s has %d markers and %d of them follow a code span: every <!--limit:id--> must come right after the `value` it guards", PagePath, len(raw), len(marks))
	}
	guarded := map[string]string{} // id -> a file that calls it
	err = filepath.WalkDir(top, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", ".claude", "spike", "tasks":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		own := filepath.Dir(path) == filepath.Join(top, "internal", "limitsdoc")
		for line := range strings.Lines(string(src)) {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // a comment that shows a call is no call
			}
			for _, m := range guardCallRE.FindAllStringSubmatch(line, -1) {
				if qualified := m[1] != ""; qualified || own {
					guarded[m[2]] = path
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range marks {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if guarded[m.ID] == "" {
			t.Errorf("%s:%d: the limit %q is on the page and no test holds it to the code: call limitsdoc.Count, Bytes, Duration or Text with the ID in the test of the package that holds the constant", PagePath, m.Line, m.ID)
		}
	}
	for id, path := range guarded {
		if !seen[id] && !strings.HasPrefix(id, "selftest.") {
			t.Errorf("%s guards the limit %q, which is not on the page", path, id)
		}
	}
}

// The values below are the chart's, not a Go package's: the page states them, and they are read from the chart's files.

func TestChartLimits(t *testing.T) {
	top, err := root()
	if err != nil {
		t.Fatal(err)
	}
	helpers, err := os.ReadFile(filepath.Join(top, "charts", "agent-kourier", "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	// The one semverCompare of the file that names the first Kubernetes minor of the custom resource route.
	m := regexp.MustCompile(`semverCompare ">=([0-9]+\.[0-9]+)\.0-0" \.Capabilities\.KubeVersion\.Version`).FindSubmatch(helpers)
	if m == nil {
		t.Fatal("charts/agent-kourier/templates/_helpers.tpl no longer has the semverCompare of the minimum Kubernetes version that this test reads")
	}
	Text(t, "minKubernetes", string(m[1]))

	raw, err := os.ReadFile(filepath.Join(top, "charts", "agent-kourier", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Resources struct {
			Limits map[string]string `json:"limits"`
		} `json:"resources"`
		TerminationGracePeriodSeconds int `json:"terminationGracePeriodSeconds"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	Text(t, "memoryLimit", values.Resources.Limits["memory"])
	Count(t, "terminationGrace", values.TerminationGracePeriodSeconds)
}
