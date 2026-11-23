package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
	"github.com/agentkourier/agent-kourier/addons/kit/store/sqlitetest"
)

// interruptedMessage is the text the kit's runtime fails a task a restart cut off with.
var interruptedMessage = runtime.InterruptedMessage("agent")

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

// The kit's SQLite store, under the agent's driver and in its words.
func TestKitSQLite(t *testing.T) {
	sqlitetest.Run(t, noun)
}

// SQLITE_PATH may name a file in a directory that does not exist yet, as the AX workspace's agent-kourier-state.
func TestSQLiteCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-kourier-state", "agent.db")
	openSQLite(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database file: %v", err)
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
	if err := s.Reader().QueryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	ms, err := migrationNames(mustSub(t, "migrations/sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(ms) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(ms))
	}
}

// The two backends' migrations are one list: the same files, translated.
func TestMigrationsInStep(t *testing.T) {
	sq, err := migrationNames(mustSub(t, "migrations/sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	pg, err := migrationNames(mustSub(t, "migrations/postgres"))
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

// migrationNames lists the migration files of fsys, in order: the kit applies them by the number their names begin with.
func migrationNames(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
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
	s := openPostgres(t, pgtest.DSN(t))
	mustLead(t, s, "a", time.Hour)
	testEveryWriteIsFenced(t, s)
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
	kitstore.SetFenceForTest(s.DB, func(fenceCtx context.Context, q kitstore.Querier) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		// The fence reads in the write's transaction, as an epoch check does.
		var one int
		if err := q.QueryRow(fenceCtx, `SELECT COUNT(*) FROM a2a_tasks WHERE id = ?`, "t1").Scan(&one); err != nil || one != 1 {
			t.Errorf("the fence's read: %d, %v", one, err)
		}
		// And writes a marker through it, which the refusal must roll back with the write: a fence that ran on another
		// connection (a reader, or a write of its own) would fail to write it or leave it behind.
		if _, err := q.Exec(fenceCtx, `INSERT INTO contexts (owner, context_id, last_active) VALUES (?, ?, ?)`,
			"fence-marker", fmt.Sprintf("marker-%d", calls), int64(calls)); err != nil {
			t.Errorf("the fence's marker: %v", err)
		}
		return errFenced
	})
	defer kitstore.SetFenceForTest(s.DB, nil)
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
		"SettleInterrupted":   func() error { _, err := s.SettleInterrupted(ctx, interruptedMessage); return err },
		"DeleteSettledBefore": func() error { _, err := s.DeleteSettledBefore(ctx, future); return err },
		"DeleteIdleContexts":  func() error { _, err := s.DeleteIdleContexts(ctx, future); return err },
		"TouchContext":        func() error { return s.TouchContext(ctx, "c1") },
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
	kitstore.SetFenceForTest(s.DB, nil)
	var markers int
	if err := s.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM contexts WHERE owner = ?`, "fence-marker").Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("the fence's markers outlived the refused writes: %d, %v; the fence ran outside their transactions", markers, err)
	}
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

// A first start cut short after the kit's migrations and before the agent's (a SIGKILL, say) leaves the kit's tables
// with no schema_migrations. The next start keeps them: they are not the old layout, whose a2a_tasks has no context_id.
func TestSQLiteKeepsTheTablesOfACutShortStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := kitstore.OpenSQLite(context.Background(), path, kitstore.Options{Noun: noun})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openSQLite(t, path)
	ctx := asCaller("agent-kourier")
	if _, err := s.Create(ctx, &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatalf("a task after the open: %v", err)
	}
	var legacy int
	if err := s.Reader().QueryRow(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'legacy_%'`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("legacy tables: %d, %v; want none", legacy, err)
	}
}

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
	if settled, err := s.SettleInterrupted(context.Background(), interruptedMessage); err != nil || len(settled.Failed) != 1 {
		t.Fatalf("SettleInterrupted = %+v, %v; want the cut-off task", settled, err)
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
	if err := s.Reader().QueryRow(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'legacy_%'`).Scan(&legacy); err != nil || legacy != 0 {
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
	mustLead(t, s, "a", time.Hour)
	var n int
	if err := s.Reader().QueryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	ms, err := migrationNames(mustSub(t, "migrations/postgres"))
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
