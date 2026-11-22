package limitsdoc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	page := "intro\n| a | `5s`<!--addon-limit:ax.reload--> | b |\n\n`2 MiB`<!--addon-limit:ax.event--> and `60s`<!--addon-limit:other.keepalive-->\n" +
		"<!--limit:triggers--> `50`<!--limit:broker-->\n"
	got := parse("p.md", page)
	want := []mark{{"ax.reload", "5s", "p.md", 2}, {"ax.event", "2 MiB", "p.md", 4}, {"other.keepalive", "60s", "p.md", 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("parse = %v, want %v", got, want)
	}
	if bad := malformed(page); len(bad) != 0 {
		t.Fatalf("malformed = %v for a page whose add-on markers all follow a code span", bad)
	}
}

func TestMalformed(t *testing.T) {
	page := strings.Join([]string{
		"`1`<!--addon-limit:ax.good-->",
		"no value <!--addon-limit:ax.bare-->",
		"`2` <!--addon-limit:ax.spaced-->",
		"`3`<!--addon-limit:ax.bad id-->",
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

// TestGuardsReportADifference holds the four guards to what they say: a value that equals the pages' passes, and one that
// does not names the page and line, what the page says and what the code has. A limit written at two places, on one page
// or on two, is checked at each; one that only another reference page has is not on the limits page, and fails.
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
		{"milliseconds", func(t testing.TB) { Duration(t, "selftest.ms", 5*time.Second) }, ""},
		{"a bad duration", func(t testing.TB) { Duration(t, "selftest.count", 7) }, "write a duration as a number and ms, s, m, h or d"},
		{"text equal", func(t testing.TB) { Text(t, "selftest.text", "1.2") }, ""},
		{"text differs", func(t testing.TB) { Text(t, "selftest.text", "1.3") }, "the code has `1.3`"},
		{"a bad unit", func(t testing.TB) { Bytes(t, "selftest.badunit", 1) }, "write a size as a number and bytes, KiB or MiB"},
		{"twice, both equal", func(t testing.TB) { Count(t, "selftest.twice", 5) }, ""},
		{"twice, one differs", func(t testing.TB) { Count(t, "selftest.twicediffer", 5) }, "limits.md:3 says `6` for the limit \"selftest.twicediffer\""},
		{"on both pages, equal", func(t testing.TB) { Duration(t, "selftest.both", 15*time.Second) }, ""},
		{"the reference page differs", func(t testing.TB) { Duration(t, "selftest.refdiffers", 10*time.Second) }, "docs/reference/x-addon.md:2 says `20s`"},
		{"only on the reference page", func(t testing.TB) { Count(t, "selftest.refonly", 1) }, "has no marker <!--addon-limit:selftest.refonly-->"},
		{"a limit not on the page", func(t testing.TB) { Count(t, "selftest.absent", 1) }, "has no marker <!--addon-limit:selftest.absent-->"},
		{"the broker's marker is not the add-on's", func(t testing.TB) { Count(t, "selftest.broker", 9) }, "has no marker <!--addon-limit:selftest.broker-->"},
	}
	withSelftestPages(t)
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

// TestRepoRoot finds the repository above the module the test works in, from a directory below the module's root.
func TestRepoRoot(t *testing.T) {
	dir := withSelftestPages(t)
	t.Chdir(filepath.Join(dir, "addons", "x", "p"))
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	// The temporary directory may be reached through a symlink (macOS's /var): compare what each resolves to.
	got, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("repoRoot = %s, want %s", got, want)
	}
}

// withSelftestPages points the guards at pages of this test's own, by making a repository of a temporary directory that
// holds a limits page and another reference page under a go.mod of an add-on module, and working in that module, as the
// add-on's tests do. It returns the repository's directory.
func withSelftestPages(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	limits := strings.Join([]string{
		"| x | `7`<!--addon-limit:selftest.count--> | `2 MiB`<!--addon-limit:selftest.bytes--> `341 KiB`<!--addon-limit:selftest.kib-->",
		"`90d`<!--addon-limit:selftest.duration--> `5000ms`<!--addon-limit:selftest.ms--> `1.2`<!--addon-limit:selftest.text-->",
		"`2 TiB`<!--addon-limit:selftest.badunit--> `5`<!--addon-limit:selftest.twice--> `5`<!--addon-limit:selftest.twicediffer--> `6`<!--addon-limit:selftest.twicediffer-->",
		"`5`<!--addon-limit:selftest.twice--> `9`<!--limit:selftest.broker--> `15s`<!--addon-limit:selftest.both--> `10s`<!--addon-limit:selftest.refdiffers-->",
	}, "\n")
	reference := strings.Join([]string{
		"| `X` | `15s`<!--addon-limit:selftest.both--> |",
		"| `Y` | `20s`<!--addon-limit:selftest.refdiffers--> | `1`<!--addon-limit:selftest.refonly--> |",
	}, "\n")
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(PagePath)), limits)
	mustWrite(t, filepath.Join(dir, "docs", "reference", "x-addon.md"), reference)
	module := filepath.Join(dir, "addons", "x")
	mustWrite(t, filepath.Join(module, "go.mod"), "module x\n")
	mustWrite(t, filepath.Join(module, "p", "p.go"), "package p\n")
	t.Chdir(module)
	return dir
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
