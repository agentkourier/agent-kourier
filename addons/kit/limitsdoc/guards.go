package limitsdoc

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// guardNames are the functions of this package that hold a limit to the code.
var guardNames = map[string]bool{"Count": true, "Bytes": true, "Duration": true, "Text": true}

// ownDir is the directory of this package below the root of its module, the kit: in it, and in no other package, a guard
// is called by its bare name.
const ownDir = "limitsdoc"

// guardCalls returns, for each limit ID that a test of the module at top holds to the code, a file that does, and the
// problems of the calls it read: an ID that is not a string literal, a value that is a literal, which would be a copy of
// the page in the test and not what the code has, a value that is a constant declared in a test file, which is a copy as
// well, and a call that no test run reaches: one under `if false`, or in a function that is neither a Test function nor
// called by name from a test file of its package (a function that only dead code calls is not seen). A test file with a
// build constraint, or for another platform, is not read: its tests do not run in every test run, so they hold nothing.
// In another package a call is limitsdoc.Count(t, "id", value); in this one (ownDir of the kit's module) it is
// Count(t, "id", value), and the IDs that begin with "selftest." are this package's own and may be given literals. The
// error is one of walking or parsing the module's files.
func guardCalls(top string) (guarded map[string]string, problems []string, err error) {
	guarded = map[string]string{}
	var files []testFile
	fset := token.NewFileSet()
	err = filepath.WalkDir(top, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "bin", "testdata":
				return filepath.SkipDir
			}
			if path != top {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // another module: its tests are not this module's guards
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
		return nil, nil, err
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
		own := dir == filepath.Join(top, ownDir)
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
	return guarded, problems, nil
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

// prefixRE is what a prefix of limit IDs must be: an add-on's name, or the kit's, and a dot.
var prefixRE = regexp.MustCompile(`^[a-z][a-z0-9-]*\.$`)

// EveryMarkerHasAGuard fails t for a marker on a reference page whose ID begins with prefix ("cma.", "ax.", "kit.") that
// no test of the module holds to the code, for a marker of the prefix on another reference page that the limits page
// does not have, for a guard of a limit that the limits page does not have, for a guard whose value is a literal, and for
// an add-on marker on any reference page that is not a code span with a value before it, which the guards could not
// read. The module is the one the test runs in: the nearest directory above the working directory with a go.mod, whose
// test files it reads (guardCalls). Each module calls it from a test of its own, with its own prefix, since only its own
// test files can hold its limits.
func EveryMarkerHasAGuard(t testing.TB, prefix string) {
	t.Helper()
	if !prefixRE.MatchString(prefix) || prefix == "selftest." {
		t.Fatalf("the prefix of limit IDs %q is not an add-on's name followed by a dot, such as \"cma.\"", prefix)
		return
	}
	top, err := repoRoot()
	if err != nil {
		t.Fatalf("find the repository root: %v", err)
		return
	}
	all, err := pages(top)
	if err != nil {
		t.Fatalf("read the reference pages: %v", err)
		return
	}
	module, err := moduleRoot()
	if err != nil {
		t.Fatalf("find the module root: %v", err)
		return
	}
	guarded, problems, err := guardCalls(module)
	if err != nil {
		t.Fatalf("read the test files of %s: %v", module, err)
		return
	}
	for _, p := range problems {
		t.Errorf("%s", p)
	}
	onLimits := map[string]bool{}
	for _, m := range parse(PagePath, all[0].text) {
		onLimits[m.ID] = true
	}
	seen := map[string]bool{}
	for _, pg := range all {
		for _, line := range malformed(pg.text) {
			t.Errorf("%s:%d has an add-on marker that the guards cannot read: write <!--addon-limit:id--> right after the `value` it guards, with an ID of letters, digits, '.', '_' and '-'", pg.path, line)
		}
		for _, m := range parse(pg.path, pg.text) {
			if !strings.HasPrefix(m.ID, prefix) || seen[m.Page+" "+m.ID] {
				continue
			}
			seen[m.Page+" "+m.ID] = true
			switch {
			case !onLimits[m.ID]:
				t.Errorf("%s:%d: the limit %q is not on %s, where every limit of the add-on is", m.Page, m.Line, m.ID, PagePath)
			case guarded[m.ID] == "":
				t.Errorf("%s:%d: the limit %q is on the page and no test holds it to the code: call limitsdoc.Count, Bytes, Duration or Text with the ID in the test of the package that holds the constant", m.Page, m.Line, m.ID)
			}
		}
	}
	for id, path := range guarded {
		if !onLimits[id] && !strings.HasPrefix(id, "selftest.") {
			t.Errorf("%s guards the limit %q, which is not on %s", path, id, PagePath)
		}
	}
}

// moduleRoot returns the root of the module the test runs in: the nearest directory above the working directory with a
// go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
