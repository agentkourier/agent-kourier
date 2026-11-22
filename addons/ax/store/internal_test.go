package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/addons/ax/store/pgtest"
)

func openSQLite(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenSQLite(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func asCaller(name string) context.Context {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx
}

func TestSQLiteIsOneAgentsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ax.db")
	first := openSQLite(t, path)
	if _, err := OpenSQLite(context.Background(), path, Options{}); !errors.Is(err, ErrInUse) {
		t.Fatalf("a second open: %v, want ErrInUse", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	openSQLite(t, path) // the lock went with the first store
}

// SQLITE_PATH may name a file in a directory that does not exist yet, as the AX workspace's agent-kourier-state.
func TestSQLiteCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-kourier-state", "agent.db")
	openSQLite(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database file: %v", err)
	}
}

func TestSQLiteRunsInWALModeWithFullSync(t *testing.T) {
	s := openSQLite(t, filepath.Join(t.TempDir(), "ax.db"))
	ctx := context.Background()
	var mode string
	if err := s.conn.reader().queryRow(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	err := s.conn.inTx(ctx, func(q querier) error {
		var sync, timeout int
		if err := q.queryRow(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
			return err
		}
		if err := q.queryRow(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			return err
		}
		if sync != 2 || timeout <= 0 {
			t.Errorf("write connection: synchronous = %d, busy_timeout = %d; want 2 (FULL) and one", sync, timeout)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsApplyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ax.db")
	s := openSQLite(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openSQLite(t, path)
	var n int
	if err := s.conn.reader().queryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	ms, err := listMigrations(mustSub(t, "migrations/sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(ms) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(ms))
	}
}

// The two backends' migrations are one list: the same files, translated.
func TestMigrationsInStep(t *testing.T) {
	sq, err := listMigrations(mustSub(t, "migrations/sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	pg, err := listMigrations(mustSub(t, "migrations/postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sq) != len(pg) {
		t.Fatalf("sqlite has %d migrations, postgres %d", len(sq), len(pg))
	}
	for i := range sq {
		if sq[i] != pg[i] {
			t.Errorf("migration %d: sqlite %+v, postgres %+v", i, sq[i], pg[i])
		}
	}
	for _, dir := range []string{"migrations/sqlite", "migrations/postgres"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "?") {
				t.Errorf("%s/%s holds a ?, which the Postgres driver would take for a placeholder", dir, e.Name())
			}
		}
	}
}

func mustSub(t *testing.T, dir string) fs.FS {
	t.Helper()
	src := sqliteMigrations
	if strings.HasSuffix(dir, "postgres") {
		src = postgresMigrations
	}
	sub, err := fs.Sub(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// errFenced stands for a deposed leader's refusal.
var errFenced = errors.New("fenced")

func TestEveryWriteIsFenced(t *testing.T) {
	testEveryWriteIsFenced(t, openSQLite(t, filepath.Join(t.TempDir(), "ax.db")))
}

func TestPostgresWritesAreFenced(t *testing.T) {
	testEveryWriteIsFenced(t, openPostgres(t, pgtest.DSN(t)))
}

// Every write, the store's and ADK's, runs the fence in its transaction: with a fence that refuses, every write fails
// and changes nothing. A leader's epoch check relies on this.
func testEveryWriteIsFenced(t *testing.T, s *Store) {
	t.Helper()
	ctx := asCaller("agent-kourier")
	seed := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := s.Create(ctx, seed); err != nil {
		t.Fatal(err)
	}
	created, err := s.Sessions().Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	s.fence = func(fenceCtx context.Context, q querier) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		// The fence reads in the write's transaction, as an epoch check does.
		var one int
		if err := q.queryRow(fenceCtx, `SELECT COUNT(*) FROM a2a_tasks WHERE id = ?`, "t1").Scan(&one); err != nil || one != 1 {
			t.Errorf("the fence's read: %d, %v", one, err)
		}
		return errFenced
	}
	defer func() { s.fence = nil }()
	future := time.Now().Add(time.Hour)
	writes := map[string]func() error{
		"Create": func() error {
			_, err := s.Create(ctx, &a2a.Task{ID: "t2", ContextID: "c1"})
			return err
		},
		"Update": func() error {
			_, err := s.Update(ctx, &taskstore.UpdateRequest{Task: seed, PrevVersion: 1})
			return err
		},
		"SettleInterrupted":   func() error { _, err := s.SettleInterrupted(ctx); return err },
		"DeleteSettledBefore": func() error { _, err := s.DeleteSettledBefore(ctx, future); return err },
		"DeleteIdleContexts":  func() error { _, err := s.DeleteIdleContexts(ctx, future); return err },
		"ADK Create": func() error {
			_, err := s.Sessions().Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c2"})
			return err
		},
		"ADK AppendEvent": func() error {
			ev := session.NewEvent(ctx, "invocation")
			ev.Author = "user"
			ev.Content = genai.NewContentFromText("hello", genai.RoleUser)
			return s.Sessions().AppendEvent(ctx, created.Session, ev)
		},
		"ADK Delete": func() error {
			return s.Sessions().Delete(ctx, &session.DeleteRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c1"})
		},
	}
	for name, write := range writes {
		before := calls
		if err := write(); !errors.Is(err, errFenced) {
			t.Errorf("%s: %v, want the fence's error", name, err)
		}
		if calls == before {
			t.Errorf("%s did not run the fence", name)
		}
	}
	s.fence = nil
	got, err := s.Get(ctx, "t1")
	if err != nil || got.Version != 1 || got.Task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("a fenced write changed the store: %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "t2"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("a fenced Create stored its task: %v", err)
	}
	sess, err := s.Sessions().Get(ctx, &session.GetRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c1"})
	if err != nil || sess.Session.Events().Len() != 0 {
		t.Fatalf("a fenced ADK write changed the session: %+v, %v", sess, err)
	}
	if _, err := s.Sessions().Get(ctx, &session.GetRequest{AppName: AppName, UserID: "agent-kourier", SessionID: "c2"}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a fenced ADK Create stored its session: %v", err)
	}
}

// legacyDDL is the layout the agent kept its tasks in before it was an add-on.
const legacyDDL = `CREATE TABLE a2a_tasks(id TEXT PRIMARY KEY, owner TEXT NOT NULL, version INTEGER NOT NULL, payload BLOB NOT NULL);
CREATE TABLE a2a_messages(owner TEXT NOT NULL, message_id TEXT NOT NULL, task_id TEXT NOT NULL, PRIMARY KEY(owner,message_id))`

// A file in the old layout keeps its tasks: a waiting question can still be answered after the upgrade, a resent
// message still joins its task, and an interrupted turn is settled.
func TestSQLiteImportsTheOldLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	u := url.URL{Scheme: "file", Path: path}
	old, err := sql.Open(sqlite.DriverName, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(context.Background(), legacyDDL); err != nil {
		t.Fatal(err)
	}
	for _, task := range []*a2a.Task{
		{ID: "asked", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}},
		{ID: "cut-off", ContextID: "c2", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
	} {
		raw, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.ExecContext(context.Background(), `INSERT INTO a2a_tasks VALUES (?, 'service', 3, ?)`, string(task.ID), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := old.ExecContext(context.Background(), `INSERT INTO a2a_messages VALUES ('service', ?, ?)`, "msg-"+string(task.ID), string(task.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := openSQLite(t, path)
	ctx := asCaller("service")
	if n, err := s.SettleInterrupted(context.Background()); err != nil || n != 1 {
		t.Fatalf("SettleInterrupted = %d, %v; want the cut-off task", n, err)
	}
	asked, err := s.Get(ctx, "asked")
	if err != nil || asked.Version != 3 || asked.Task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("the waiting question: %+v, %v", asked, err)
	}
	if got, err := s.MessageTask(ctx, "msg-asked"); err != nil || got == nil || got.ID != "asked" {
		t.Fatalf("its message: %+v, %v", got, err)
	}
	if cut, err := s.Get(ctx, "cut-off"); err != nil || cut.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("the cut-off turn: %+v, %v", cut, err)
	}
	if resp, err := s.List(ctx, &a2a.ListTasksRequest{ContextID: "c1"}); err != nil || len(resp.Tasks) != 1 {
		t.Fatalf("List by context: %+v, %v", resp, err)
	}
	if n, err := s.DeleteIdleContexts(context.Background(), time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("DeleteIdleContexts = %d, %v; want both imported contexts, idle from the import on", n, err)
	}
	var legacy int
	if err := s.conn.reader().queryRow(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'legacy_%'`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("legacy tables left: %d, %v", legacy, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	openSQLite(t, path) // a second open finds the new layout and imports nothing
}

func openPostgres(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := OpenPostgres(context.Background(), dsn, Options{})
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Replicas that start together on an empty database each apply the migrations, the store's and ADK's, once between
// them.
func TestPostgresReplicasMigrateTogether(t *testing.T) {
	dsn := pgtest.DSN(t)
	const replicas = 4
	errs := make(chan error, replicas)
	stores := make(chan *Store, replicas)
	for range replicas {
		go func() {
			s, err := OpenPostgres(context.Background(), dsn, Options{})
			errs <- err
			stores <- s
		}()
	}
	for range replicas {
		if err := <-errs; err != nil {
			t.Errorf("OpenPostgres: %v", err)
		}
		if s := <-stores; s != nil {
			t.Cleanup(func() { _ = s.Close() })
		}
	}
	s := openPostgres(t, dsn)
	var n int
	if err := s.conn.reader().queryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	ms, err := listMigrations(mustSub(t, "migrations/postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(ms) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(ms))
	}
	if _, err := s.Sessions().Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "c"}); err != nil {
		t.Fatalf("ADK's tables: %v", err)
	}
}

func TestPostgresErrorsHideTheDSN(t *testing.T) {
	const secret = "fake-password-do-not-print"
	for _, dsn := range []string{"postgres://u:" + secret + "@[::1:5432/db", "host=127.0.0.1 port=1 user=u password=" + secret + " connect_timeout=1"} {
		_, err := OpenPostgres(context.Background(), dsn, Options{})
		if err == nil {
			t.Fatal("OpenPostgres accepted it")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error shows the password: %v", err)
		}
	}
}
