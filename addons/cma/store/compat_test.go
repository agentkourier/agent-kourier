package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

// A database the adapter created before its store moved onto the kit: testdata/main holds the migrations it ran then,
// copied from that version's SQL, which kept the leadership table in the adapter's migration 0002. The kit's migration
// adopts that table; nothing is lost or recreated, and the lease still fences.

// applyMain migrates a database as the adapter did before the kit: each file of testdata/main/<engine> in its own
// statement, and its version in schema_migrations.
func applyMain(t *testing.T, engine string, exec func(query string, args ...any) error) {
	t.Helper()
	dir := filepath.Join("testdata", "main", engine)
	names, err := migrationNames(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at bigint NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (` + strconv.Itoa(i+1) + `, 0)`); err != nil {
			t.Fatal(err)
		}
	}
}

// seedMain writes the adapter's rows in the main layout: a task paused on a question, the message that started it,
// and its context's session.
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

// assertMainRowsKept checks that the rows seedMain wrote read back as they were, and that the adapter's version table
// is the one the main layout left.
func assertMainRowsKept(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	got, err := s.Get(ctx, "t1")
	if err != nil || got.Version != 3 || got.Task.Status.State != a2a.TaskStateInputRequired || got.Task.ContextID != "c1" {
		t.Fatalf("the paused task: %+v, %v", got, err)
	}
	if id, ok, err := s.MessageTask(ctx, "m1"); err != nil || !ok || id != "t1" {
		t.Fatalf("its message: %v, %v, %v", id, ok, err)
	}
	sessions, err := s.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ContextID != "c1" || sessions[0].SessionID != "sesn_1" || !sessions[0].LastActive.Equal(time.Unix(0, 42)) {
		t.Fatalf("its session: %+v, %v", sessions, err)
	}
	var versions []int
	err = s.Reader().Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`, nil, func(scan func(...any) error) error {
		var v int
		if err := scan(&v); err != nil {
			return err
		}
		versions = append(versions, v)
		return nil
	})
	if err != nil || !slices.Equal(versions, []int{1, 2}) {
		t.Fatalf("schema_migrations: %v, %v; want the main layout's 1 and 2", versions, err)
	}
	var kit int
	if err := s.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM kit_schema_migrations`).Scan(&kit); err != nil || kit != 1 {
		t.Fatalf("kit_schema_migrations: %d rows, %v; want the kit's one", kit, err)
	}
}

func TestSQLiteOpensADatabaseOfMain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cma.db")
	ctx := context.Background()
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error { _, err := raw.ExecContext(ctx, query, args...); return err }
	applyMain(t, "sqlite", exec)
	seedMain(t, exec, false)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	for range 2 { // the second open finds every migration recorded
		s := openSQLite(t, path)
		assertMainRowsKept(t, s)
		if _, _, err := s.TryLead(ctx, "a", "http://a:8090", time.Minute); !errors.Is(err, kitstore.ErrNoElection) {
			t.Fatalf("TryLead on SQLite: %v, want ErrNoElection", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s := openSQLite(t, path)
	if err := s.RecordMessage(ctx, "m2", "t1"); err != nil {
		t.Fatalf("a write after the open: %v", err)
	}
}

func TestPostgresOpensADatabaseOfMain(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	exec := func(query string, args ...any) error { _, err := raw.Exec(ctx, query, args...); return err }
	applyMain(t, "postgres", exec)
	seedMain(t, exec, true)
	// A lease that a replica of the main layout holds, live.
	if err := exec(`UPDATE leadership SET holder = 'old-pod', address = 'http://old-pod:8090', epoch = 7, expires_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	var oid uint32
	if err := raw.QueryRow(ctx, `SELECT 'leadership'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatal(err)
	}

	a := openPostgres(t, dsn)
	assertMainRowsKept(t, a)
	if l, err := a.CurrentLeader(ctx); err != nil || l != (kitstore.Lease{Holder: "old-pod", Address: "http://old-pod:8090", Epoch: 7, Live: true}) {
		t.Fatalf("the lease after the open: %+v, %v", l, err)
	}
	var after uint32
	if err := raw.QueryRow(ctx, `SELECT 'leadership'::regclass::oid`).Scan(&after); err != nil || after != oid {
		t.Fatalf("the leadership table is %d (%v), was %d: it was recreated", after, err, oid)
	}

	// The lease still fences: a replica that has not taken it writes nothing, the old holder's live lease keeps
	// another out, the same holder takes it with the next epoch, and a later take deposes it.
	if err := a.RecordMessage(ctx, "m2", "t1"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("a write before the take: %v, want ErrDeposed", err)
	}
	if _, ok, err := a.TryLead(ctx, "a", "http://a:8090", time.Minute); err != nil || ok {
		t.Fatalf("TryLead while old-pod's lease is live: %v, %v; want no take", ok, err)
	}
	if l := mustLead(t, a, "old-pod", time.Minute); l.Epoch != 8 {
		t.Fatalf("epoch %d after the main layout's 7, want 8", l.Epoch)
	}
	if err := a.RecordMessage(ctx, "m2", "t1"); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	b := openPostgres(t, dsn)
	assertMainRowsKept(t, b)
	if l := mustLead(t, b, "old-pod", time.Minute); l.Epoch != 9 {
		t.Fatalf("epoch %d, want 9", l.Epoch)
	}
	if err := a.RecordMessage(ctx, "m3", "t1"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the deposed leader's write: %v, want ErrDeposed", err)
	}
	if err := b.RecordMessage(ctx, "m3", "t1"); err != nil {
		t.Fatalf("the new leader's write: %v", err)
	}
}
