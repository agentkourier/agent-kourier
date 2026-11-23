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
	"github.com/glebarez/sqlite"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver, for the fixture's connection
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

// A database the agent created before its store moved onto the kit: testdata/main holds the migrations it ran then,
// copied from that version's SQL, which kept the leadership table in the agent's migration 0002. The kit's migration
// adopts that table; nothing is lost or recreated, ADK's session included, and the lease still fences.

// seedMain migrates raw as the agent did before the kit (each file of testdata/main/<engine> in its own statement, and
// its version in schema_migrations, then ADK's migration) and writes the agent's rows in that layout: a task paused on
// a question, the message that started it, its context and the context's ADK session.
func seedMain(t *testing.T, raw *sql.DB, engine string, dialector gorm.Dialector) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if engine == "postgres" {
			query = numberedForTest(query)
		}
		if _, err := raw.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	dir := filepath.Join("testdata", "main", engine)
	names, err := migrationNames(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at bigint NOT NULL)`)
	for i, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 0)`, i+1)
	}
	payload, err := json.Marshal(&a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}})
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO a2a_tasks (id, context_id, owner, version, state, status_at, updated_at, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"t1", "c1", "agent-kourier", 3, string(a2a.TaskStateInputRequired), nil, 42, string(payload))
	exec(`INSERT INTO message_ids (owner, message_id, task_id, created_at) VALUES (?, ?, ?, ?)`, "agent-kourier", "m1", "t1", 42)
	exec(`INSERT INTO contexts (owner, context_id, last_active) VALUES (?, ?, ?)`, "agent-kourier", "c1", 42)
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := database.NewSessionServiceFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(sessions); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c1"}); err != nil {
		t.Fatal(err)
	}
}

// numberedForTest numbers the ? placeholders of query for Postgres.
func numberedForTest(query string) string {
	out, n := "", 0
	for _, r := range query {
		if r == '?' {
			n++
			out += "$" + strconv.Itoa(n)
			continue
		}
		out += string(r)
	}
	return out
}

// assertMainRowsKept checks that the rows seedMain wrote read back as they were, and that the agent's version table
// is the one the main layout left.
func assertMainRowsKept(t *testing.T, s *Store) {
	t.Helper()
	ctx := asCaller("agent-kourier")
	got, err := s.Get(ctx, "t1")
	if err != nil || got.Version != 3 || got.Task.Status.State != a2a.TaskStateInputRequired || got.Task.ContextID != "c1" {
		t.Fatalf("the paused task: %+v, %v", got, err)
	}
	if task, err := s.MessageTask(ctx, "m1"); err != nil || task == nil || task.ID != "t1" {
		t.Fatalf("its message: %+v, %v", task, err)
	}
	var active int64
	if err := s.Reader().QueryRow(ctx, `SELECT last_active FROM contexts WHERE owner = ? AND context_id = ?`, "agent-kourier", "c1").Scan(&active); err != nil || active != 42 {
		t.Fatalf("its context: %d, %v", active, err)
	}
	if _, err := s.Sessions().Get(ctx, &session.GetRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c1"}); err != nil {
		t.Fatalf("its ADK session: %v", err)
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
	path := filepath.Join(t.TempDir(), "ax.db")
	ctx := context.Background()
	raw, err := sql.Open(sqlite.DriverName, (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	seedMain(t, raw, "sqlite", sqlite.Dialector{Conn: raw})
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
	if err := s.TouchContext(asCaller("agent-kourier"), "c1"); err != nil {
		t.Fatalf("a write after the open: %v", err)
	}
	if err := createSession(s, "c2"); err != nil {
		t.Fatalf("an ADK write after the open: %v", err)
	}
}

func TestPostgresOpensADatabaseOfMain(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	seedMain(t, raw, "postgres", postgres.New(postgres.Config{Conn: raw}))
	// A lease that a replica of the main layout holds, live.
	if _, err := raw.ExecContext(ctx, `UPDATE leadership SET holder = 'old-pod', address = 'http://old-pod:8090', epoch = 7, expires_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	var oid uint32
	if err := raw.QueryRowContext(ctx, `SELECT 'leadership'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatal(err)
	}

	a := openPostgres(t, dsn)
	assertMainRowsKept(t, a)
	if l, err := a.CurrentLeader(ctx); err != nil || l != (kitstore.Lease{Holder: "old-pod", Address: "http://old-pod:8090", Epoch: 7, Live: true}) {
		t.Fatalf("the lease after the open: %+v, %v", l, err)
	}
	var after uint32
	if err := raw.QueryRowContext(ctx, `SELECT 'leadership'::regclass::oid`).Scan(&after); err != nil || after != oid {
		t.Fatalf("the leadership table is %d (%v), was %d: it was recreated", after, err, oid)
	}

	// The lease still fences, the store's writes and ADK's: a replica that has not taken it writes nothing, the old
	// holder's live lease keeps another out, the same holder takes it with the next epoch, and a later take deposes it.
	caller := asCaller("agent-kourier")
	if err := a.TouchContext(caller, "c1"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("a write before the take: %v, want ErrDeposed", err)
	}
	if _, ok, err := a.TryLead(ctx, "a", "http://a:8090", time.Minute); err != nil || ok {
		t.Fatalf("TryLead while old-pod's lease is live: %v, %v; want no take", ok, err)
	}
	if l := mustLead(t, a, "old-pod", time.Minute); l.Epoch != 8 {
		t.Fatalf("epoch %d after the main layout's 7, want 8", l.Epoch)
	}
	if err := a.TouchContext(caller, "c1"); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	if err := createSession(a, "c2"); err != nil {
		t.Fatalf("the leader's ADK write: %v", err)
	}
	b := openPostgres(t, dsn)
	if got, err := b.Get(caller, "t1"); err != nil || got.Version != 3 {
		t.Fatalf("the paused task, on a second replica: %+v, %v", got, err)
	}
	if l := mustLead(t, b, "old-pod", time.Minute); l.Epoch != 9 {
		t.Fatalf("epoch %d, want 9", l.Epoch)
	}
	if err := a.TouchContext(caller, "c1"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the deposed leader's write: %v, want ErrDeposed", err)
	}
	if err := createSession(a, "c3"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the deposed leader's ADK write: %v, want ErrDeposed", err)
	}
	if err := b.TouchContext(caller, "c1"); err != nil {
		t.Fatalf("the new leader's write: %v", err)
	}
}
