package limitsdoc

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestGuardCallsReadsTheSyntax holds guardCalls to what it promises, on files of its own: a literal value and an ID that is not a
// literal are problems, a call in a comment is no call, and a file with a build constraint or for another platform is not read.
func TestGuardCallsReadsTheSyntax(t *testing.T) {
	top := t.TempDir()
	mustWrite(t, filepath.Join(top, "go.mod"), "module x\n")
	pkg := filepath.Join(top, "p")
	mustWrite(t, filepath.Join(pkg, "a_test.go"), "package p\n\nfunc TestF(t *testing.T) {\n\t// limitsdoc.Count(t, \"incomment\", maxTriggers)\n\tlimitsdoc.Count(t, \"good\", maxTriggers)\n\tlimitsdoc.Count(t, \"copied\", 50)\n\tlimitsdoc.Count(t, id, maxTriggers)\n}\n")
	mustWrite(t, filepath.Join(pkg, "typed_test.go"), "package p\n\nfunc TestK(t *testing.T) {\n\tlimitsdoc.Count(t, \"paren\", (50))\n\tlimitsdoc.Duration(t, \"units\", 90*24*time.Hour)\n\tlimitsdoc.Count(t, \"conv\", int(24*time.Hour))\n\tlimitsdoc.Count(t, \"sum\", 40+10)\n\tlimitsdoc.Duration(t, \"dur\", time.Duration(5)*time.Second)\n\tlimitsdoc.Count(t, \"named\", maxTriggers+1)\n\tlimitsdoc.Count(t, \"field\", cfg.Limit)\n\tlimitsdoc.Count(t, \"call\", len(f()))\n\tlimitsdoc.Count(t, \"convnamed\", int(maxTriggers))\n}\n")
	mustWrite(t, filepath.Join(pkg, "reach_test.go"), "package p\n\nconst copiedLimit = 50\n\nfunc TestReach(t *testing.T) {\n\tconst local = 7\n\tlimitsdoc.Count(t, \"testconst\", copiedLimit)\n\tlimitsdoc.Count(t, \"localconst\", local+1)\n\tlimitsdoc.Count(t, \"prodconst\", maxTriggers)\n\tif false {\n\t\tlimitsdoc.Count(t, \"underfalse\", maxTriggers)\n\t}\n\tif true {\n\t\tlimitsdoc.Count(t, \"undertrue\", maxTriggers)\n\t} else if false {\n\t\tlimitsdoc.Count(t, \"elsefalse\", maxTriggers)\n\t}\n\tif false {\n\t} else {\n\t\tlimitsdoc.Count(t, \"elselive\", maxTriggers)\n\t}\n\tcalledHelper(t)\n}\n\nfunc calledHelper(t *testing.T) { limitsdoc.Count(t, \"calledhelper\", maxTriggers) }\n\nfunc uncalledHelper(t *testing.T) { limitsdoc.Count(t, \"uncalled\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "testdata", "x_test.go"), "package p\n\nfunc d() { limitsdoc.Count(t, \"intestdata\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(top, "nested", "go.mod"), "module nested\n")
	mustWrite(t, filepath.Join(top, "nested", "n_test.go"), "package n\n\nfunc n() { limitsdoc.Count(t, \"innested\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "tagged_test.go"), "//go:build testhooks\n\npackage p\n\nfunc g() { limitsdoc.Count(t, \"tagged\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "other_plan9_test.go"), "package p\n\nfunc h() { limitsdoc.Count(t, \"plan9\", maxTriggers) }\n")
	guarded, problems, err := guardCalls(top)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"incomment", "tagged", "plan9", "intestdata", "innested"} {
		if _, ok := guarded[id]; ok {
			t.Errorf("%q is guarded, but its call is in a comment, behind a build constraint or in a file for another platform", id)
		}
	}
	if _, ok := guarded["good"]; !ok {
		t.Error("a plain call was not read")
	}
	joined := strings.Join(problems, "\n")
	for _, id := range []string{"copied", "paren", "units", "conv", "sum", "dur"} {
		if !strings.Contains(joined, `"`+id+`" is given a literal`) {
			t.Errorf("the value of %q is written out and was not refused: %q", id, problems)
		}
	}
	for _, id := range []string{"named", "field", "call", "convnamed", "good"} {
		if strings.Contains(joined, `"`+id+`" is given a literal`) {
			t.Errorf("the value of %q is the code's own and was refused: %q", id, problems)
		}
	}
	for id, want := range map[string]string{
		"testconst":  "is given a constant that a test file declares",
		"localconst": "is given a constant that a test file declares",
		"underfalse": "is under `if false`",
		"elsefalse":  "is under `if false`",
		"uncalled":   "which is not a test and which no test file of its package calls",
	} {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, strconv.Quote(id)) && strings.Contains(p, want) }) {
			t.Errorf("%q was not refused with %q: %q", id, want, problems)
		}
	}
	for _, id := range []string{"prodconst", "undertrue", "elselive", "calledhelper"} {
		if _, ok := guarded[id]; !ok || strings.Contains(joined, strconv.Quote(id)) {
			t.Errorf("%q is the code's own, in a test that runs: guarded = %v, and problems %q", id, ok, problems)
		}
	}
	for _, id := range []string{"underfalse", "elsefalse", "uncalled"} {
		if _, ok := guarded[id]; ok {
			t.Errorf("%q is guarded, but no test run reaches its call", id)
		}
	}
	if !strings.Contains(joined, "must be a string literal") || len(problems) != 12 {
		t.Errorf("problems = %q, want six values written out, one ID that is not a literal, two constants of test files, two guards under `if false`, and one in a function that nothing calls", problems)
	}
}

// TestEveryMarkerHasAGuard holds the kit's own limits, whose IDs begin with "kit.", to its tests.
func TestEveryMarkerHasAGuard(t *testing.T) {
	EveryMarkerHasAGuard(t, "kit.")
}

// TestEveryMarkerHasAGuardReadsEveryPage runs the check for an add-on module of its own, x, beside another add-on's
// marker (y.), on a limits page and another reference page: it reads both pages, fails for each marker of the prefix
// that no test holds, that only another page has, or that it cannot read, and for each guard of a limit that the limits
// page does not have (x.refonly, which only the other page has, and x.gone), and leaves the other add-on's markers alone.
func TestEveryMarkerHasAGuardReadsEveryPage(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(PagePath)), strings.Join([]string{
		"| a | `5`<!--addon-limit:x.guarded--> |",
		"| b | `6`<!--addon-limit:x.unguarded--> |",
		"| c | `7`<!--addon-limit:y.other--> |",
		"| d | `8`<!--addon-limit:x.both--> |",
	}, "\n"))
	mustWrite(t, filepath.Join(dir, "docs", "reference", "x-addon.md"), strings.Join([]string{
		"| `8`<!--addon-limit:x.both--> |",
		"| `9`<!--addon-limit:x.refonly--> |",
		"| `10`<!--addon-limit:y.refonly--> |",
		"| no value <!--addon-limit:x.bare--> |",
	}, "\n"))
	module := filepath.Join(dir, "addons", "x")
	mustWrite(t, filepath.Join(module, "go.mod"), "module x\n")
	mustWrite(t, filepath.Join(module, "p", "p_test.go"), "package p\n\nfunc TestLimits(t *testing.T) {\n"+
		"\tlimitsdoc.Count(t, \"x.guarded\", maxA)\n\tlimitsdoc.Count(t, \"x.both\", maxB)\n"+
		"\tlimitsdoc.Count(t, \"x.refonly\", maxC)\n\tlimitsdoc.Count(t, \"x.gone\", maxD)\n}\n")
	t.Chdir(filepath.Join(module, "p"))

	r := &recorder{TB: t}
	EveryMarkerHasAGuard(r, "x.")
	joined := strings.Join(r.errs, "\n")
	for _, want := range []string{
		`docs/reference/limits.md:2: the limit "x.unguarded" is on the page and no test holds it to the code`,
		`docs/reference/x-addon.md:2: the limit "x.refonly" is not on docs/reference/limits.md, where every limit of the add-on is`,
		`guards the limit "x.refonly", which is not on docs/reference/limits.md`,
		`guards the limit "x.gone", which is not on docs/reference/limits.md`,
		"docs/reference/x-addon.md:4 has an add-on marker that the guards cannot read",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("reported %q, want it to contain %q", joined, want)
		}
	}
	for _, id := range []string{"x.guarded", "x.both", "y.other", "y.refonly"} {
		if strings.Contains(joined, strconv.Quote(id)) {
			t.Errorf("reported %q, which names %q: it is guarded or another add-on's", joined, id)
		}
	}
	if len(r.errs) != 5 || r.fatal {
		t.Errorf("reported %d problems (fatal %v), want the five above: %q", len(r.errs), r.fatal, r.errs)
	}

	// The same module and pages for the add-on y: its markers are unguarded, and x's are not its own; the guard of a
	// limit that the page does not have is the module's whatever the prefix.
	r = &recorder{TB: t}
	EveryMarkerHasAGuard(r, "y.")
	joined = strings.Join(r.errs, "\n")
	for _, want := range []string{
		`docs/reference/limits.md:3: the limit "y.other" is on the page and no test holds it to the code`,
		`docs/reference/x-addon.md:3: the limit "y.refonly" is not on docs/reference/limits.md`,
		`guards the limit "x.gone", which is not on docs/reference/limits.md`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("for y.: reported %q, want it to contain %q", joined, want)
		}
	}
	if strings.Contains(joined, `"x.unguarded"`) {
		t.Errorf("for y.: reported %q, which names x's marker", joined)
	}
}

// TestEveryMarkerHasAGuardRefusesABadPrefix fails at once for a prefix that is not a lower-case name and a dot, which would
// select no add-on's markers, or every add-on's.
func TestEveryMarkerHasAGuardRefusesABadPrefix(t *testing.T) {
	for _, prefix := range []string{"", "cma", ".", "CMA.", "cma.x.", "selftest."} {
		r := &recorder{TB: t}
		EveryMarkerHasAGuard(r, prefix)
		if !r.fatal || len(r.errs) != 1 || !strings.Contains(r.errs[0], "prefix") {
			t.Errorf("prefix %q: reported %q (fatal %v), want one fatal error about the prefix", prefix, r.errs, r.fatal)
		}
	}
}
