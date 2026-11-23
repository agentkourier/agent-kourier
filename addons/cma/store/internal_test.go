package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
	"github.com/agentkourier/agent-kourier/addons/kit/store/sqlitetest"
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

// The kit's SQLite store, under the adapter's driver and in its words.
func TestKitSQLite(t *testing.T) {
	sqlitetest.Run(t, noun)
}

func TestMigrationsApplyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cma.db")
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

// Every write goes through Write, so the fence sees it: with a fence that refuses, every write fails and changes
// nothing. A leader's epoch check (on Postgres) relies on this.
func TestEveryWriteIsFenced(t *testing.T) {
	s := openSQLite(t, filepath.Join(t.TempDir(), "cma.db"))
	testEveryWriteIsFenced(t, s)
}

func testEveryWriteIsFenced(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	seed := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	if _, err := s.Create(ctx, seed); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	kitstore.SetFenceForTest(s.DB, func(context.Context, kitstore.Querier) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
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
		"DeleteSettledBefore": func() error { _, err := s.DeleteSettledBefore(ctx, future); return err },
		"RecordMessage":       func() error { return s.RecordMessage(ctx, "m1", "t1") },
		"PutSession":          func() error { return s.PutSession(ctx, Session{ContextID: "c1", SessionID: "sesn_1"}) },
		"TouchSession":        func() error { return s.TouchSession(ctx, "c1", "sesn_1", future) },
		"ForgetContext":       func() error { return s.ForgetContext(ctx, "c1", "sesn_1", future) },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, errFenced) {
			t.Errorf("%s: %v, want the fence's error", name, err)
		}
	}
	if calls != len(writes) {
		t.Errorf("the fence ran %d times for %d writes", calls, len(writes))
	}
	kitstore.SetFenceForTest(s.DB, nil)
	got, err := s.Get(ctx, "t1")
	if err != nil || got.Version != 1 {
		t.Fatalf("a fenced write changed the store: %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "t2"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("a fenced Create stored its task: %v", err)
	}
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

func TestPostgresWritesAreFenced(t *testing.T) {
	s := openPostgres(t, pgtest.DSN(t))
	mustLead(t, s, "a", time.Hour)
	testEveryWriteIsFenced(t, s)
}

// Replicas that start together on an empty database each apply the migrations once between them.
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
