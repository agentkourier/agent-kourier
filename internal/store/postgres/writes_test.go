package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// writeSQL matches a statement that changes data: one that begins with INSERT, UPDATE or DELETE, or a WITH
// that holds one (a data-modifying CTE).
var writeSQL = regexp.MustCompile(`(?is)^\s*(insert|update|delete)\b|^\s*with\b.*\b(insert|update|delete)\b`)

// sqlText resolves the SQL an argument stands for: a string literal, a constant of the package, or a
// concatenation of them. ok is false for anything else.
func sqlText(e ast.Expr, consts map[string]ast.Expr) (text string, ok bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(x.Value)
		return v, err == nil
	case *ast.Ident:
		if c, found := consts[x.Name]; found {
			return sqlText(c, consts)
		}
	case *ast.BinaryExpr:
		l, lok := sqlText(x.X, consts)
		r, rok := sqlText(x.Y, consts)
		if lok && rok {
			return l + r, true
		}
		if lok { // a constant followed by something spliced in (a column list): the start is what tells the verb
			return l, true
		}
	case *ast.ParenExpr:
		return sqlText(x.X, consts)
	}
	return "", false
}

// TestWritesGoThroughInTx: no code of the package starts a transaction or runs a statement that writes
// on the pool except inTx and the migrations. H4-5 puts the leader's epoch check in inTx, and relies on
// this: a write that went round it would not be fenced. Reads (Query, QueryRow) may use the pool, but not
// with SQL that changes data (INSERT ... RETURNING through QueryRow is a write all the same).
func TestWritesGoThroughInTx(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// What the pool offers that writes, or that begins what does, and the pgx helpers that begin a transaction.
	poolWrites := map[string]bool{"Exec": true, "Begin": true, "BeginTx": true, "CopyFrom": true, "SendBatch": true, "Acquire": true, "AcquireFunc": true}
	pgxBegins := map[string]bool{"BeginFunc": true, "BeginTxFunc": true}
	allowed := map[string]bool{
		"inTx":           true, // the one path of every write
		"applyMigration": true, // runs before there is a store, under its own lock
	}
	// Calls that run SQL on the pool, and which argument holds it. listInteractions passes its argument on, so
	// it is checked at its callers, and its own call may not know the SQL.
	poolQueries := map[string]int{"Query": 1, "QueryRow": 1}
	helperQueries := map[string]int{"listInteractions": 2}

	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]ast.Expr{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, file)
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) == len(vs.Values) {
					for i, n := range vs.Names {
						consts[n.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	if len(parsed) < 10 {
		t.Fatalf("checked %d source files; the scan is not looking at the package", len(parsed))
	}

	checkSQL := func(call *ast.CallExpr, fn, callee string, arg int) {
		if arg >= len(call.Args) {
			return
		}
		text, ok := sqlText(call.Args[arg], consts)
		switch {
		case !ok && fn != "listInteractions":
			t.Errorf("%s: %s passes %s SQL this test cannot read: use a literal or a constant", fset.Position(call.Pos()), fn, callee)
		case ok && writeSQL.MatchString(text):
			t.Errorf("%s: %s runs a statement that changes data through %s: a write must go through inTx", fset.Position(call.Pos()), fn, callee)
		}
	}
	for _, file := range parsed {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || allowed[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				method := sel.Sel.Name
				onPool, onStore, onPgx := false, false, false
				switch x := sel.X.(type) {
				case *ast.SelectorExpr: // s.pool.Exec(...)
					onPool = x.Sel.Name == "pool"
				case *ast.Ident: // pool.Exec(...), pgx.BeginFunc(...), s.listInteractions(...)
					onPool, onStore, onPgx = x.Name == "pool", x.Name == "s", x.Name == "pgx"
				}
				if (onPool && poolWrites[method]) || (onPgx && pgxBegins[method]) {
					t.Errorf("%s: %s calls %s: a write must go through inTx", fset.Position(call.Pos()), fn.Name.Name, method)
				}
				if arg, ok := poolQueries[method]; ok && onPool {
					checkSQL(call, fn.Name.Name, "pool."+method, arg)
				}
				if arg, ok := helperQueries[method]; ok && onStore {
					checkSQL(call, fn.Name.Name, "s."+method, arg)
				}
				return true
			})
		}
	}
}
