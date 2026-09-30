package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var (
	_  store.Store = (*Store)(nil)
	t0             = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
)

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "courier.db"))
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	var mode string
	var fk int
	if err := s.read.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if err := s.write.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", fk, err)
	}
	e := store.Event{ID: "ev1", Kind: "alert", BindingKey: "ns/b", Payload: []byte{0, 1, 2}, ReceivedAt: t0}

	for _, bad := range []store.Event{{ReceivedAt: t0}, {ID: "x"}} {
		if _, err := s.AppendEvent(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("AppendEvent(%+v) = %v; want ErrInvalid", bad, err)
		}
	}
	if ins, err := s.AppendEvent(ctx, e); err != nil || !ins {
		t.Fatalf("first append = %v, %v; want true, nil", ins, err)
	}
	dup := e
	dup.Payload = []byte("changed")
	if ins, err := s.AppendEvent(ctx, dup); err != nil || ins {
		t.Fatalf("duplicate append = %v, %v; want false, nil", ins, err)
	}
	got, err := s.GetEvent(ctx, "ev1")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if !bytes.Equal(got.Payload, e.Payload) || got.Kind != "alert" || got.BindingKey != "ns/b" ||
		!got.ReceivedAt.Equal(t0) || got.Status != store.EventPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(t0) {
		t.Fatalf("stored event = %+v", got)
	}

	if err := s.SetEventStatus(ctx, "ev1", store.EventDead, "boom"); err != nil {
		t.Fatalf("SetEventStatus: %v", err)
	}
	if got, _ = s.GetEvent(ctx, "ev1"); got.Status != store.EventDead || got.LastError != "boom" {
		t.Fatalf("after status update = %+v", got)
	}
	if err := s.SetEventStatus(ctx, "ev1", "bogus", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("SetEventStatus bogus = %v; want ErrInvalid", err)
	}
	if _, err := s.GetEvent(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetEvent missing = %v; want ErrNotFound", err)
	}
	if err := s.SetEventStatus(ctx, "nope", store.EventDone, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetEventStatus missing = %v; want ErrNotFound", err)
	}
}

func TestMigrateAppliesInOrderOnceAndRollsBackFailures(t *testing.T) {
	ctx := context.Background()
	// A bare pool, so the fake versions cannot collide with the real migrations.
	db, err := openPool(ctx, filepath.Join(t.TempDir(), "migrate.db"), 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatalf("openPool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	good := fstest.MapFS{"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")}}
	broken := fstest.MapFS{
		"migrations/0001_a.sql": good["migrations/0001_a.sql"],
		"migrations/0002_b.sql": {Data: []byte("CREATE TABLE b (x INTEGER); NOT SQL;")},
	}
	if err := migrate(ctx, db, broken); err == nil {
		t.Fatal("migrate with a broken migration returned nil")
	}
	if err := migrate(ctx, db, good); err != nil { // idempotent: 0001 is already applied
		t.Fatalf("re-running an applied migration: %v", err)
	}
	var v1, v2, hasA, hasB int
	err = db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM schema_migrations WHERE version = 1),
		(SELECT count(*) FROM schema_migrations WHERE version = 2),
		(SELECT count(*) FROM sqlite_master WHERE name = 'a'),
		(SELECT count(*) FROM sqlite_master WHERE name = 'b')`).Scan(&v1, &v2, &hasA, &hasB)
	if err != nil || v1 != 1 || v2 != 0 || hasA != 1 || hasB != 0 {
		t.Fatalf("v1=%d v2=%d tableA=%d tableB=%d, %v; want 1, 0, 1, 0", v1, v2, hasA, hasB, err)
	}
}

func TestListMigrationsRejectsDuplicateVersionsAndSortsNumerically(t *testing.T) {
	ok := fstest.MapFS{"migrations/10_b.sql": {}, "migrations/9_a.sql": {}}
	ms, err := listMigrations(ok)
	if err != nil || len(ms) != 2 || ms[0].version != 9 || ms[1].version != 10 {
		t.Fatalf("listMigrations = %+v, %v; want 9 then 10", ms, err)
	}
	dup := fstest.MapFS{"migrations/0002_a.sql": {}, "migrations/2_b.sql": {}}
	if _, err := listMigrations(dup); err == nil {
		t.Fatal("duplicate version accepted")
	}
}
