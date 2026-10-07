package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

// policyFragments are the texts of the policy predicates (predicates.go) that an adapter must not write itself, in
// either dialect: what is pending and what is settled, what holds a session back, when a session is past its horizon,
// the open and closed states of a trigger thread, and the byte order of the outbox's event index.
var policyFragments = []string{
	"message_ref = ''", "abandoned_at = 0", "abandoned_at <> 0", "abandoned_at > 0", "abandoned_at IS",
	"last_activity <", "NOT EXISTS (SELECT 1 FROM interactions", "NOT EXISTS (SELECT 1 FROM outbox", `COLLATE "C"`,
	"closed_at IS", "closed_at > 0", "closed_at <> 0", "closed_at = 0 AND", "cooldown_until IS", "cooldown_until > 0",
	"cooldown_until <> 0", "cooldown_until = 0 AND", "decided_at IS NOT NULL", "decided_at > 0", "octet_length(", "length(request",
	"state = 'pending'", "state <> 'pending'", "delivered_at IS NULL", "delivered_at IS NOT NULL",
}

// policyExceptions are the places where an adapter writes one of the fragments on purpose, each with its reason. They are
// statements of the adapter that are not the policy of the predicate: a compare-and-set guard of a state transition (the
// row must still be pending when the update runs), a read of one row's own state, or a query that has no predicate to
// share (a replay of every undelivered reply by thread, or a lookup by ID).
var policyExceptions = map[string]map[string]string{
	"sqlite/interaction_transitions.go": {"state = 'pending'": "the compare-and-set guard of a decision"},
	"postgres/interactions.go": {
		"state = 'pending'":    "the compare-and-set guard of a decision, and the ListPending and text-reply lookups of one thread",
		"delivered_at IS NULL": "the compare-and-set guard of MarkInteractionDelivered",
	},
	"sqlite/interactions.go": {"state = 'pending'": "the ListPending and text-reply lookups of one thread"},
	"sqlite/replies.go":      {"delivered_at IS NULL": "the head of a thread's reply queue, and the stranded-reply sweep"},
	"postgres/replies.go":    {"delivered_at IS NULL": "the head of a thread's reply queue, and the stranded-reply sweep"},
	"sqlite/recovery.go":     {"delivered_at IS NULL": "recovery's list of the threads with a reply queued"},
	"postgres/sessions.go":   {"delivered_at IS NULL": "recovery's list of the threads with a reply queued"},
	"sqlite/presses.go":      {"state = 'pending'": "a press's own interaction", "delivered_at IS NULL": "the replies queued for one interaction"},
	"postgres/presses.go":    {"state = 'pending'": "a press's own interaction", "delivered_at IS NULL": "the replies queued for one interaction"},
}

// TestAdaptersDoNotSpellThePolicyPredicates is a search of the adapters' source text, not of their syntax: a backend that
// writes a fragment of a policy predicate in its own SQL has a second copy, and the copies drift. The predicates are
// store's, rendered through the backend's Dialect (dialect.go); the adapters keep the shape of their statements. It only
// finds the spellings it lists, so a new predicate or a new spelling of one belongs in policyFragments; the exceptions
// are policyExceptions.
func TestAdaptersDoNotSpellThePolicyPredicates(t *testing.T) {
	for file, fragments := range policyExceptions { // an exception that no longer applies is dropped
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for f := range fragments {
			if !strings.Contains(string(src), f) {
				t.Errorf("%s is excepted for %q, which it no longer writes", file, f)
			}
		}
	}
	for _, dir := range adapterDirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no files in %s: %v", dir, err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") || filepath.Base(file) == "dialect.go" {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range policyFragments {
				if _, ok := policyExceptions[filepath.ToSlash(file)][f]; !ok && strings.Contains(string(src), f) {
					t.Errorf("%s writes %q: the predicate is store's (predicates.go)", file, f)
				}
			}
		}
	}
}
