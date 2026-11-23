package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
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

// asKourier is a context whose call runs as the adapter's one caller, as the caller check runs every call.
func asKourier() context.Context {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(auth.Caller, nil)
	return ctx
}

// The kit's SQLite store, under the adapter's driver and in its words.
func TestKitSQLite(t *testing.T) {
	sqlitetest.Run(t, noun)
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
	ctx := asKourier()
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
		"SettleInterrupted":   func() error { _, err := s.SettleInterrupted(ctx, "interrupted"); return err },
		"DeleteSettledBefore": func() error { _, err := s.DeleteSettledBefore(ctx, future); return err },
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

// kitMigrations counts the migrations the kit has recorded on s's database.
func kitMigrations(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.Reader().QueryRow(context.Background(), `SELECT COUNT(*) FROM kit_schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
	got, want := kitMigrations(t, openPostgres(t, dsn)), kitMigrations(t, openPostgres(t, pgtest.DSN(t)))
	if got != want {
		t.Fatalf("kit_schema_migrations has %d rows, want the %d one store records", got, want)
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
