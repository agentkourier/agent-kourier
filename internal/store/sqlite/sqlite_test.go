package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// newInteraction returns a valid interaction created at t0 that is due after d.
func newInteraction(id string, d time.Duration) store.Interaction {
	return store.Interaction{
		ID: id, Key: key, TaskID: "task1", Kind: store.InteractionAskUser,
		Request: []byte(`{"q":1}`), CreatedAt: t0, Deadline: t0.Add(d),
	}
}

// migrated is an empty database with every migration applied, as the bytes of its file, made once for the package's
// tests: opening an empty one runs the migrations, which under the race detector is about a second of CPU, and most
// tests need a store and not that. (Package sqlitetest does the same for other packages; this package's own tests
// cannot import it, since it imports this one.) The tests of the migrations open an empty database with openAt.
// A write-once memo in test code, not state of the product. It is sqlitetest's template, made the same way: keep the two
// alike (internal/store/sqlite/sqlitetest/sqlitetest.go).
var migrated = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "sqlite-migrated-")
	if err != nil {
		return nil, fmt.Errorf("make the directory of the template: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("migrate the template: %w", err)
	}
	if err := s.Close(); err != nil { // the last connection to close folds the write-ahead log into the file
		return nil, fmt.Errorf("close the template: %w", err)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, errors.New("the template left a write-ahead log: its file is not the whole database")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the template: %w", err)
	}
	return raw, nil
})

// seededPath returns a path, in the test's temporary directory, of a database that is migrated already.
func seededPath(t *testing.T) string {
	t.Helper()
	raw, err := migrated()
	if err != nil {
		t.Fatalf("migrate the template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "courier.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	return openAt(t, seededPath(t))
}

// Every connection of both pools runs in WAL mode with foreign keys on, which the rest of the package takes for
// granted.
func TestOpenSetsTheJournalModeAndForeignKeys(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	var mode string
	var fk int
	if err := s.read.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if err := s.write.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", fk, err)
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
