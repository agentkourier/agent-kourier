package limitsdoc

import (
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// guardNames are the functions of this package that hold a limit to the code.
var guardNames = map[string]bool{"Count": true, "Bytes": true, "Duration": true, "Text": true}

// guardCalls returns, for each limit ID that a test of the repository holds to the code, a file that does, and the problems
// of the calls it read: an ID that is not a string literal, and a value that is a literal, which would be a copy of the page
// in the test and not what the code has. A test file with a build constraint, or for another platform, is not read: its tests
// do not run in every test run, so they hold nothing. In another package a call is limitsdoc.Count(t, "id", value); in this one
// it is Count(t, "id", value), and the IDs that begin with "selftest." are this package's own and may be given literals.
func guardCalls(t *testing.T, top string) (guarded map[string]string, problems []string) {
	t.Helper()
	guarded = map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(top, func(path string, d os.DirEntry, err error) error {
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
		if ok, err := build.Default.MatchFile(filepath.Dir(path), d.Name()); err != nil || !ok {
			return err // a name for another platform, or a constraint that this build does not meet
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, group := range file.Comments {
			if group.End() > file.Package {
				break
			}
			for _, c := range group.List {
				if constraint.IsGoBuild(c.Text) {
					return nil // tagged: it does not run in every test run
				}
			}
		}
		own := filepath.Dir(path) == filepath.Join(top, "internal", "limitsdoc")
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok && x.Name == "limitsdoc" {
					name = fun.Sel.Name
				}
			case *ast.Ident:
				if own {
					name = fun.Name
				}
			}
			if !guardNames[name] || len(call.Args) != 3 {
				return true
			}
			at := fset.Position(call.Pos()).String()
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				problems = append(problems, at+": the ID of a limit must be a string literal, so that the page can be matched to the test")
				return true
			}
			id, err := strconv.Unquote(lit.Value)
			if err != nil {
				problems = append(problems, at+": "+err.Error())
				return true
			}
			if _, literal := call.Args[2].(*ast.BasicLit); literal && !strings.HasPrefix(id, "selftest.") {
				problems = append(problems, at+": the limit "+strconv.Quote(id)+" is given a literal: pass the constant or the default that the code uses, or the test holds the page to a copy of it")
			}
			guarded[id] = path
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return guarded, problems
}

// TestEveryMarkerHasAGuard fails for a marker of the page that no test of the repository holds to the code, for a guard whose
// value is a literal, and for a marker that is not a code span with a value before it, which the guards could not read.
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
	guarded, problems := guardCalls(t, top)
	for _, p := range problems {
		t.Error(p)
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

// TestGuardCallsReadsTheSyntax holds guardCalls to what it promises, on files of its own: a literal value and an ID that is not a
// literal are problems, a call in a comment is no call, and a file with a build constraint or for another platform is not read.
func TestGuardCallsReadsTheSyntax(t *testing.T) {
	top := t.TempDir()
	mustWrite(t, filepath.Join(top, "go.mod"), "module x\n")
	pkg := filepath.Join(top, "p")
	mustWrite(t, filepath.Join(pkg, "a_test.go"), "package p\n\nfunc f() {\n\t// limitsdoc.Count(t, \"incomment\", maxTriggers)\n\tlimitsdoc.Count(t, \"good\", maxTriggers)\n\tlimitsdoc.Count(t, \"copied\", 50)\n\tlimitsdoc.Count(t, id, maxTriggers)\n}\n")
	mustWrite(t, filepath.Join(pkg, "tagged_test.go"), "//go:build testhooks\n\npackage p\n\nfunc g() { limitsdoc.Count(t, \"tagged\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "other_plan9_test.go"), "package p\n\nfunc h() { limitsdoc.Count(t, \"plan9\", maxTriggers) }\n")
	guarded, problems := guardCalls(t, top)
	for _, id := range []string{"incomment", "tagged", "plan9"} {
		if _, ok := guarded[id]; ok {
			t.Errorf("%q is guarded, but its call is in a comment, behind a build constraint or in a file for another platform", id)
		}
	}
	if _, ok := guarded["good"]; !ok {
		t.Error("a plain call was not read")
	}
	joined := strings.Join(problems, "\n")
	if len(problems) != 2 || !strings.Contains(joined, `"copied" is given a literal`) || !strings.Contains(joined, "must be a string literal") {
		t.Errorf("problems = %q, want a literal value and an ID that is not a literal", problems)
	}
}
