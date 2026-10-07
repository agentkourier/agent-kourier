package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// adapterDirs are the backends of store.Store. Each is SQL, transactions and what the backend alone needs; the policy is
// store.Guard's (guard.go).
var adapterDirs = []string{"sqlite", "postgres"}

// exemptions are the places where a backend may say what the guard says, each with its reason.
var exemptions = map[string]string{
	// Postgres text holds no NUL and only valid UTF-8; only the driver knows what it refused.
	"postgres/postgres.go classify": "maps the driver's refusal of text it cannot store to ErrInvalid",
	// Lead is not an operation of store.Store, so the guard does not see it.
	"postgres/leadership.go Lead": "checks the holder of an epoch, which is not in store.Store",
	// SetSessionTask has no trace in its arguments, so the adapter reads the context's, which is the guard's span.
	"sqlite/session_task.go SetSessionTask": "writes the trace of the call's context into the session's row",
	"postgres/sessions.go SetSessionTask":   "writes the trace of the call's context into the session's row",
}

// TestAdaptersDoNotCheckArgumentsOrRecordTelemetry: a backend that validates what it is given, or starts a span for an
// operation, has a second copy of what the guard does, and the two drift. The adapters are held to SQL: no reference to
// ErrInvalid, no telemetry.Start, no trace stamping of a row, no redaction of an entry, no key check of their own.
func TestAdaptersDoNotCheckArgumentsOrRecordTelemetry(t *testing.T) {
	for _, dir := range adapterDirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no files in %s: %v", dir, err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fn.Name.Name == "validKey" {
					t.Errorf("%s defines validKey: the key rule is store.Guard's", file)
				}
				where := filepath.ToSlash(file) + " " + fn.Name.Name
				if _, ok := exemptions[where]; ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkg, _ := sel.X.(*ast.Ident)
					switch {
					case pkg != nil && pkg.Name == "store" && sel.Sel.Name == "ErrInvalid":
						t.Errorf("%s uses store.ErrInvalid: the guard refuses illegal arguments before the adapter is called", where)
					case pkg != nil && pkg.Name == "telemetry" && (sel.Sel.Name == "Start" || sel.Sel.Name == "StartKind" || sel.Sel.Name == "Capture"):
						t.Errorf("%s calls telemetry.%s: the guard records the operation and stamps the row", where, sel.Sel.Name)
					case sel.Sel.Name == "Redacted":
						t.Errorf("%s redacts an entry: the guard hands the adapter the redacted one", where)
					}
					return true
				})
			}
		}
	}
}
