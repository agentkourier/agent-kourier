package limitsdoc

import (
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// guardNames are the functions of this package that hold a limit to the code.
var guardNames = map[string]bool{"Count": true, "Bytes": true, "Duration": true, "Text": true}

// guardCalls returns, for each limit ID that a test of the repository holds to the code, a file that does, and the problems
// of the calls it read: an ID that is not a string literal, a value that is a literal, which would be a copy of the page in the
// test and not what the code has, a value that is a constant declared in a test file, which is a copy as well, and a call that
// no test run reaches: one under `if false`, or in a function that is neither a Test function nor called by name from a test file
// of its package (a function that only dead code calls is not seen). A test file with a build constraint, or for another
// platform, is not read: its tests do not run in every test run, so they hold nothing. In another package a call is
// limitsdoc.Count(t, "id", value); in this one it is Count(t, "id", value), and the IDs that begin with "selftest." are this
// package's own and may be given literals.
func guardCalls(t *testing.T, top string) (guarded map[string]string, problems []string) {
	t.Helper()
	guarded = map[string]string{}
	var files []testFile
	fset := token.NewFileSet()
	err := filepath.WalkDir(top, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", ".claude", "spike", "tasks", "testdata":
				return filepath.SkipDir
			}
			if path != top {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // another module (examples/ax): its tests are not this repository's guards
				}
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
		files = append(files, testFile{path, file})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// What each package's test files declare and call, which a guard is judged against.
	testConsts, called := map[string]map[string]bool{}, map[string]map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f.path)
		if testConsts[dir] == nil {
			testConsts[dir], called[dir] = map[string]bool{}, map[string]bool{}
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GenDecl:
				if n.Tok == token.CONST {
					for _, spec := range n.Specs {
						for _, name := range spec.(*ast.ValueSpec).Names {
							testConsts[dir][name.Name] = true
						}
					}
				}
			case *ast.CallExpr:
				switch fun := n.Fun.(type) {
				case *ast.Ident:
					called[dir][fun.Name] = true
				case *ast.SelectorExpr:
					called[dir][fun.Sel.Name] = true
				}
			}
			return true
		})
	}
	for _, f := range files {
		dir := filepath.Dir(f.path)
		own := dir == filepath.Join(top, "internal", "limitsdoc")
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			reached := isTestFunc(fn) || called[dir][fn.Name.Name]
			var visit func(n ast.Node, live bool)
			visit = func(root ast.Node, live bool) {
				ast.Inspect(root, func(n ast.Node) bool {
					if ifs, ok := n.(*ast.IfStmt); ok {
						if cond, isIdent := ifs.Cond.(*ast.Ident); isIdent && cond.Name == "false" {
							visit(ifs.Body, false) // nothing in the body runs
							if ifs.Else != nil {
								visit(ifs.Else, live)
							}
							return false
						}
					}
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
					selftest := strings.HasPrefix(id, "selftest.")
					switch {
					case !live:
						problems = append(problems, at+": the guard of the limit "+strconv.Quote(id)+" is under `if false`, so no test run reaches it")
						return true
					case !reached:
						problems = append(problems, at+": the guard of the limit "+strconv.Quote(id)+" is in the function "+fn.Name.Name+", which is not a test and which no test file of its package calls, so no test run reaches it")
						return true
					case onlyLiterals(call.Args[2]) && !selftest:
						problems = append(problems, at+": the limit "+strconv.Quote(id)+" is given a literal: pass the constant or the default that the code uses, or the test holds the page to a copy of it")
					case !selftest && namesAny(call.Args[2], testConsts[dir]):
						problems = append(problems, at+": the limit "+strconv.Quote(id)+" is given a constant that a test file declares: pass the constant or the default that the code uses, or the test holds the page to a copy of it")
					}
					guarded[id] = f.path
					return true
				})
			}
			visit(fn.Body, true)
		}
	}
	return guarded, problems
}

// testFile is a test file that guardCalls reads.
type testFile struct {
	path string
	file *ast.File
}

// isTestFunc reports whether fn is a Test function of package testing: a name that begins with Test and a *testing.T parameter.
func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "T"
}

// namesAny reports whether e uses an identifier that is in names, as a value: the field of a selector (cfg.Limit) and the name
// of a function that is called are not.
func namesAny(e ast.Expr, names map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			found = found || namesAny(n.X, names)
			return false
		case *ast.CallExpr:
			for _, a := range n.Args {
				found = found || namesAny(a, names)
			}
			return false
		case *ast.Ident:
			found = found || names[n.Name]
		}
		return true
	})
	return found
}

// onlyLiterals reports whether e is a number written out: literals, and the units of package time, joined by arithmetic, in
// parentheses or under a conversion, such as (50), 90*24*time.Hour or int(24*time.Hour). It holds nothing that the code names.
func onlyLiterals(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return onlyLiterals(e.X)
	case *ast.UnaryExpr:
		return onlyLiterals(e.X)
	case *ast.BinaryExpr:
		return onlyLiterals(e.X) && onlyLiterals(e.Y)
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && x.Name == "time"
	case *ast.CallExpr:
		if len(e.Args) != 1 {
			return false
		}
		switch fun := e.Fun.(type) {
		case *ast.Ident: // int(...), int64(...)
			return onlyLiterals(e.Args[0])
		case *ast.SelectorExpr: // time.Duration(...)
			x, ok := fun.X.(*ast.Ident)
			return ok && x.Name == "time" && onlyLiterals(e.Args[0])
		}
	}
	return false
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
	mustWrite(t, filepath.Join(pkg, "a_test.go"), "package p\n\nfunc TestF(t *testing.T) {\n\t// limitsdoc.Count(t, \"incomment\", maxTriggers)\n\tlimitsdoc.Count(t, \"good\", maxTriggers)\n\tlimitsdoc.Count(t, \"copied\", 50)\n\tlimitsdoc.Count(t, id, maxTriggers)\n}\n")
	mustWrite(t, filepath.Join(pkg, "typed_test.go"), "package p\n\nfunc TestK(t *testing.T) {\n\tlimitsdoc.Count(t, \"paren\", (50))\n\tlimitsdoc.Duration(t, \"units\", 90*24*time.Hour)\n\tlimitsdoc.Count(t, \"conv\", int(24*time.Hour))\n\tlimitsdoc.Count(t, \"sum\", 40+10)\n\tlimitsdoc.Duration(t, \"dur\", time.Duration(5)*time.Second)\n\tlimitsdoc.Count(t, \"named\", maxTriggers+1)\n\tlimitsdoc.Count(t, \"field\", cfg.Limit)\n\tlimitsdoc.Count(t, \"call\", len(f()))\n\tlimitsdoc.Count(t, \"convnamed\", int(maxTriggers))\n}\n")
	mustWrite(t, filepath.Join(pkg, "reach_test.go"), "package p\n\nconst copiedLimit = 50\n\nfunc TestReach(t *testing.T) {\n\tconst local = 7\n\tlimitsdoc.Count(t, \"testconst\", copiedLimit)\n\tlimitsdoc.Count(t, \"localconst\", local+1)\n\tlimitsdoc.Count(t, \"prodconst\", maxTriggers)\n\tif false {\n\t\tlimitsdoc.Count(t, \"underfalse\", maxTriggers)\n\t}\n\tif true {\n\t\tlimitsdoc.Count(t, \"undertrue\", maxTriggers)\n\t} else if false {\n\t\tlimitsdoc.Count(t, \"elsefalse\", maxTriggers)\n\t}\n\tif false {\n\t} else {\n\t\tlimitsdoc.Count(t, \"elselive\", maxTriggers)\n\t}\n\tcalledHelper(t)\n}\n\nfunc calledHelper(t *testing.T) { limitsdoc.Count(t, \"calledhelper\", maxTriggers) }\n\nfunc uncalledHelper(t *testing.T) { limitsdoc.Count(t, \"uncalled\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "testdata", "x_test.go"), "package p\n\nfunc d() { limitsdoc.Count(t, \"intestdata\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(top, "nested", "go.mod"), "module nested\n")
	mustWrite(t, filepath.Join(top, "nested", "n_test.go"), "package n\n\nfunc n() { limitsdoc.Count(t, \"innested\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "tagged_test.go"), "//go:build testhooks\n\npackage p\n\nfunc g() { limitsdoc.Count(t, \"tagged\", maxTriggers) }\n")
	mustWrite(t, filepath.Join(pkg, "other_plan9_test.go"), "package p\n\nfunc h() { limitsdoc.Count(t, \"plan9\", maxTriggers) }\n")
	guarded, problems := guardCalls(t, top)
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
