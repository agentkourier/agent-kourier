// Package limitsdoc ties the add-on's rows of docs/reference/limits.md to the add-on's code. A value on that page that is
// a constant of the add-on (or a default that its code fills in) is written as a code span followed at once by a marker,
// `4000`<!--addon-limit:cma.tool-result-runes-->. A test in the package that holds the constant calls Count, Bytes,
// Duration or Text with the marker's ID and the constant, and fails when the page says something else, at each place the
// page writes that ID. So a constant cannot change without the page, and TestEveryMarkerHasAGuard fails for a marker of
// this add-on that no test of its module calls.
//
// It is the add-on module's copy of the broker's internal/limitsdoc, which this module cannot import, with the same value
// formats. The broker's guard reads <!--limit:id--> markers and leaves these alone.
//
// The page is read from the repository, found from the test's working directory, so these functions are for tests.
package limitsdoc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// PagePath is the limits page, relative to the repository root.
const PagePath = "docs/reference/limits.md"

// IDPrefix begins the ID of each limit of this add-on. A marker with another prefix is another add-on's.
const IDPrefix = "cma."

// markerRE finds a marker and the code span before it, which holds the value.
var markerRE = regexp.MustCompile("`([^`\n]+)`<!--addon-limit:([A-Za-z0-9._-]+)-->")

// anyMarkerRE finds every add-on marker, with or without a value before it, so that a malformed one is seen.
var anyMarkerRE = regexp.MustCompile(`<!--addon-limit:[^>]*-->`)

// mark is one marker of the page: the limit's ID, the value written before it, and the line it is on.
type mark struct {
	ID, Value string
	Line      int
}

// parse returns the add-on markers of page in order.
func parse(page string) []mark {
	var out []mark
	for _, m := range markerRE.FindAllStringSubmatchIndex(page, -1) {
		out = append(out, mark{
			ID: page[m[4]:m[5]], Value: page[m[2]:m[3]],
			Line: 1 + strings.Count(page[:m[0]], "\n"),
		})
	}
	return out
}

// malformed returns the lines of the add-on markers of page that parse cannot read: one with no code span right before
// it, or with an ID of other characters.
func malformed(page string) []int {
	read := map[int]bool{}
	for _, m := range markerRE.FindAllStringIndex(page, -1) {
		read[m[1]] = true
	}
	var lines []int
	for _, m := range anyMarkerRE.FindAllStringIndex(page, -1) {
		if !read[m[1]] {
			lines = append(lines, 1+strings.Count(page[:m[0]], "\n"))
		}
	}
	return lines
}

// repoRoot returns the repository root: the nearest directory above the working directory that holds the page. The
// add-on's go.mod is below it.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(PagePath))); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s above %s", PagePath, dir)
		}
		dir = parent
	}
}

// read returns the markers of the page that have the ID, and fails the test if there is none.
func read(t testing.TB, id string) []mark {
	t.Helper()
	top, err := repoRoot()
	if err != nil {
		t.Fatalf("find the repository root: %v", err)
		return nil
	}
	page, err := os.ReadFile(filepath.Join(top, filepath.FromSlash(PagePath))) //nolint:gosec // a fixed path under the repository
	if err != nil {
		t.Fatalf("read the limits page: %v", err)
		return nil
	}
	var found []mark
	for _, m := range parse(string(page)) {
		if m.ID == id {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		t.Fatalf("%s has no marker <!--addon-limit:%s-->: write the limit's value on the page, in a code span followed by the marker", PagePath, id)
	}
	return found
}

// Count fails the test if the page says another number than got for the limit id.
func Count(t testing.TB, id string, got int) {
	t.Helper()
	for _, m := range read(t, id) {
		n, err := strconv.Atoi(m.Value)
		if err != nil || strconv.Itoa(n) != m.Value || n != got {
			t.Errorf("%s:%d says `%s` for the limit %q (a plain number), the code has %d", PagePath, m.Line, m.Value, id, got)
		}
	}
}

var bytesRE = regexp.MustCompile(`^([0-9]+) (bytes|KiB|MiB)$`)

// Bytes fails the test if the page says another size than got bytes for the limit id. A size is written as a number and
// a unit: bytes, KiB or MiB.
func Bytes(t testing.TB, id string, got int) {
	t.Helper()
	units := map[string]int{"bytes": 1, "KiB": 1 << 10, "MiB": 1 << 20}
	for _, m := range read(t, id) {
		sub := bytesRE.FindStringSubmatch(m.Value)
		if sub == nil {
			t.Errorf("%s:%d says `%s` for the limit %q: write a size as a number and bytes, KiB or MiB", PagePath, m.Line, m.Value, id)
			continue
		}
		n, _ := strconv.Atoi(sub[1])
		if want := n * units[sub[2]]; want != got {
			t.Errorf("%s:%d says `%s` (%d bytes) for the limit %q, the code has %d bytes", PagePath, m.Line, m.Value, want, id, got)
		}
	}
}

var durationRE = regexp.MustCompile(`^([0-9]+)(ms|s|m|h|d)$`)

// Duration fails the test if the page says another length of time than got for the limit id. A duration is written as
// a number and one unit: ms, s, m, h or d (a day of 24 hours).
func Duration(t testing.TB, id string, got time.Duration) {
	t.Helper()
	units := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	for _, m := range read(t, id) {
		sub := durationRE.FindStringSubmatch(m.Value)
		if sub == nil {
			t.Errorf("%s:%d says `%s` for the limit %q: write a duration as a number and ms, s, m, h or d", PagePath, m.Line, m.Value, id)
			continue
		}
		n, _ := strconv.Atoi(sub[1])
		if want := time.Duration(n) * units[sub[2]]; want != got {
			t.Errorf("%s:%d says `%s` (%s) for the limit %q, the code has %s", PagePath, m.Line, m.Value, want, id, got)
		}
	}
}

// Text fails the test if the page says other text than got for the limit id.
func Text(t testing.TB, id, got string) {
	t.Helper()
	for _, m := range read(t, id) {
		if m.Value != got {
			t.Errorf("%s:%d says `%s` for the limit %q, the code has `%s`", PagePath, m.Line, m.Value, id, got)
		}
	}
}
