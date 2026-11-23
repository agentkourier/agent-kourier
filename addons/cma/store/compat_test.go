package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver, for the fixture's connection

	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

// A database an earlier build of the adapter wrote: testdata/main holds the migrations it ran, copied from that
// version's SQL, which kept each context's session in a table of the adapter's (context_sessions). This build keeps
// sessions as the kit's context refs and does not migrate that layout: the open fails with ErrEarlierLayout, before
// the kit's migrations, and leaves every table and row as it was.

// applyMain migrates a database as the earlier adapter did: each file of testdata/main/<engine> in its own statement,
// and its version in schema_migrations.
func applyMain(t *testing.T, engine string, exec func(query string, args ...any) error) {
	t.Helper()
	dir := filepath.Join("testdata", "main", engine)
	entries, err := os.ReadDir(dir) // sorted by name: the order the migrations ran in
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at bigint NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(string(body)); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if err := exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (` + strconv.Itoa(i+1) + `, 0)`); err != nil {
			t.Fatal(err)
		}
	}
}

// seedMain writes the adapter's rows in the earlier layout: a task paused on a question, the message that started
// it, and its context's session.
func seedMain(t *testing.T, exec func(query string, args ...any) error, numbered bool) {
	t.Helper()
	raw, err := json.Marshal(&a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO a2a_tasks (id, context_id, owner, version, state, status_at, updated_at, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{"t1", "c1", "agent-kourier", 3, string(a2a.TaskStateInputRequired), nil, 42, string(raw)}},
		{`INSERT INTO message_ids (message_id, task_id, created_at) VALUES (?, ?, ?)`, []any{"m1", "t1", 42}},
		{`INSERT INTO context_sessions (context_id, session_id, last_active) VALUES (?, ?, ?)`, []any{"c1", "sesn_1", int64(42)}},
	} {
		query := row.query
		if numbered {
			query = pgPlaceholders(query)
		}
		if err := exec(query, row.args...); err != nil {
			t.Fatalf("%s: %v", row.query, err)
		}
	}
}

// pgPlaceholders numbers the ? placeholders of query for pgx.
func pgPlaceholders(query string) string {
	out, n := []rune{}, 0
	for _, r := range query {
		if r == '?' {
			n++
			out = append(out, []rune("$"+strconv.Itoa(n))...)
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// layout is a database's tables, each with its row count.
type layout map[string]int

// snapshot reads the layout of raw's database: the tables that tables lists, and the rows of each.
func snapshot(t *testing.T, raw *sql.DB, tables string) layout {
	t.Helper()
	out := layout{}
	for _, name := range tableNames(t, raw, tables) {
		var n int
		if err := raw.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	return out
}

func tableNames(t *testing.T, raw *sql.DB, tables string) []string {
	t.Helper()
	rows, err := raw.QueryContext(context.Background(), tables)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// assertRefused checks that open refuses the earlier layout with ErrEarlierLayout, at each try, and changes nothing.
func assertRefused(t *testing.T, raw *sql.DB, tables string, open func() (*Store, error)) {
	t.Helper()
	before := snapshot(t, raw, tables)
	if before["context_sessions"] != 1 || before["a2a_tasks"] != 1 {
		t.Fatalf("the earlier layout = %v, want its seeded rows", before)
	}
	for range 2 { // a restart finds it refused again
		s, err := open()
		if !errors.Is(err, ErrEarlierLayout) {
			if s != nil {
				_ = s.Close()
			}
			t.Fatalf("the open: %v, want ErrEarlierLayout", err)
		}
	}
	if after := snapshot(t, raw, tables); !maps.Equal(after, before) {
		t.Fatalf("the refused open changed the database: %v, was %v", after, before)
	}
}

func TestSQLiteRefusesAnEarlierLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cma.db")
	ctx := context.Background()
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	raw.SetMaxOpenConns(1)
	exec := func(query string, args ...any) error { _, err := raw.ExecContext(ctx, query, args...); return err }
	applyMain(t, "sqlite", exec)
	seedMain(t, exec, false)
	assertRefused(t, raw, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`,
		func() (*Store, error) { return OpenSQLite(ctx, path, Options{}) })
}

func TestPostgresRefusesAnEarlierLayout(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	exec := func(query string, args ...any) error { _, err := raw.ExecContext(ctx, query, args...); return err }
	applyMain(t, "postgres", exec)
	seedMain(t, exec, true)
	assertRefused(t, raw, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY table_name`,
		func() (*Store, error) { return OpenPostgres(ctx, dsn, Options{}) })
}
