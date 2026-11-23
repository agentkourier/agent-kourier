// Package sqlitetest holds the kit's SQLite store to its contract under an add-on's driver: the kit links none, so its
// own tests cannot open SQLite, and each add-on runs Run from a test of its store package, which links the driver that
// registers "sqlite". Only tests import it.
package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/store"
)

// Run runs the conformance tests. noun is the add-on's store.Options.Noun, whose words the errors must keep.
func Run(t *testing.T, noun string) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, noun string)
	}{
		{"OneProcessPerFile", oneProcessPerFile},
		{"WALModeFullSyncBusyTimeout", walModeFullSyncBusyTimeout},
		{"MigrationsApplyOnceKitsFirst", migrationsApplyOnceKitsFirst},
		{"ElectsNoLeader", electsNoLeader},
		{"NoFenceUnlessSet", noFenceUnlessSet},
		{"WritesRunTheFenceInTheirTransaction", writesRunTheFenceInTheirTransaction},
		{"MigrationLockRunsTheMigration", migrationLockRunsTheMigration},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, noun) })
	}
}

func open(t *testing.T, path, noun string) *store.DB {
	t.Helper()
	db, err := store.OpenSQLite(context.Background(), path, store.Options{Noun: noun})
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// notes is an add-on's migrations: one table for the tests to write.
var notes = fstest.MapFS{"0001_notes.sql": {Data: []byte(`CREATE TABLE notes (id text PRIMARY KEY)`)}}

func openWithNotes(t *testing.T, path, noun string) *store.DB {
	t.Helper()
	db := open(t, path, noun)
	if err := db.Migrate(context.Background(), notes); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func count(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// A second process on the file is refused, in the add-on's words, and the lock goes with the first one's Close.
func oneProcessPerFile(t *testing.T, noun string) {
	path := filepath.Join(t.TempDir(), "addon.db")
	first := open(t, path, noun)
	_, err := store.OpenSQLite(context.Background(), path, store.Options{Noun: noun})
	if !errors.Is(err, store.ErrInUse) {
		t.Fatalf("a second open: %v, want ErrInUse", err)
	}
	if want := "open sqlite store " + `"` + path + `"` + ": another " + noun + " has the SQLite file open"; err.Error() != want {
		t.Fatalf("a second open: %q, want %q", err, want)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	open(t, path, noun) // the lock went with the first database
}

func walModeFullSyncBusyTimeout(t *testing.T, noun string) {
	db := open(t, filepath.Join(t.TempDir(), "addon.db"), noun)
	ctx := context.Background()
	var mode string
	if err := db.Reader().QueryRow(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	err := db.Write(ctx, func(q store.Querier) error {
		var sync, timeout int
		if err := q.QueryRow(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			return err
		}
		if sync != 2 || int64(timeout) != store.SQLiteBusyTimeout.Milliseconds() {
			t.Errorf("write connection: synchronous = %d, busy_timeout = %d; want 2 (FULL) and %d", sync, timeout, store.SQLiteBusyTimeout.Milliseconds())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The kit's migrations go in a version table of their own, before the add-on's, and a reopen applies neither again.
func migrationsApplyOnceKitsFirst(t *testing.T, noun string) {
	path := filepath.Join(t.TempDir(), "addon.db")
	db := openWithNotes(t, path, noun)
	kit := count(t, db, `SELECT COUNT(*) FROM kit_schema_migrations`)
	if kit == 0 {
		t.Fatal("kit_schema_migrations records no migration")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema_migrations has %d rows, want the add-on's one", n)
	}
	var kitAt, addonAt int64
	if err := db.Reader().QueryRow(context.Background(), `SELECT MAX(applied_at) FROM kit_schema_migrations`).Scan(&kitAt); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader().QueryRow(context.Background(), `SELECT MIN(applied_at) FROM schema_migrations`).Scan(&addonAt); err != nil {
		t.Fatal(err)
	}
	if kitAt > addonAt {
		t.Fatalf("the kit's migrations were applied at %d, after the add-on's at %d", kitAt, addonAt)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openWithNotes(t, path, noun)
	if n := count(t, db, `SELECT COUNT(*) FROM kit_schema_migrations`); n != kit {
		t.Fatalf("kit_schema_migrations has %d rows after a reopen, want %d", n, kit)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema_migrations has %d rows after a reopen, want 1", n)
	}
}

func electsNoLeader(t *testing.T, noun string) {
	db := open(t, filepath.Join(t.TempDir(), "addon.db"), noun)
	ctx := context.Background()
	if _, _, err := db.TryLead(ctx, "a", "http://a:8090", time.Minute); !errors.Is(err, store.ErrNoElection) {
		t.Fatalf("TryLead on SQLite: %v, want ErrNoElection", err)
	}
	for name, err := range map[string]error{"Renew": db.Renew(ctx, time.Minute), "Release": db.Release(ctx)} {
		if !errors.Is(err, store.ErrNoElection) {
			t.Errorf("%s on SQLite: %v, want ErrNoElection", name, err)
		}
	}
	if _, err := db.CurrentLeader(ctx); !errors.Is(err, store.ErrNoElection) {
		t.Errorf("CurrentLeader on SQLite: %v, want ErrNoElection", err)
	}
}

// SQLite is one replica's store: its writes, Write's and a library's on SQL, are never fenced.
func noFenceUnlessSet(t *testing.T, noun string) {
	db := openWithNotes(t, filepath.Join(t.TempDir(), "addon.db"), noun)
	ctx := context.Background()
	if err := db.Write(ctx, func(q store.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO notes (id) VALUES (?)`, "n1")
		return err
	}); err != nil {
		t.Fatalf("a SQLite write is never fenced: %v", err)
	}
	if err := inSQLTx(ctx, db, func(tx *sql.Tx) error { return db.Fence(ctx, tx) }); err != nil {
		t.Fatalf("Fence on SQLite: %v, want none", err)
	}
}

// inSQLTx runs fn in a transaction on db.SQL(), as a library's write would, and commits it when fn returns nil.
func inSQLTx(ctx context.Context, db *store.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// errFenced stands for a deposed leader's refusal.
var errFenced = errors.New("fenced")

// Write and Fence run the fence in the write's own transaction: a marker the fence writes goes with the refused write.
// A fence that ran on another connection would fail to write it (the one write connection is the transaction's) or
// leave it behind.
func writesRunTheFenceInTheirTransaction(t *testing.T, noun string) {
	db := openWithNotes(t, filepath.Join(t.TempDir(), "addon.db"), noun)
	ctx := context.Background()
	calls := 0
	store.SetFenceForTest(db, func(ctx context.Context, q store.Querier) error {
		calls++
		if _, err := q.Exec(ctx, `INSERT INTO notes (id) VALUES (?)`, "marker-"+strings.Repeat("x", calls)); err != nil {
			t.Errorf("the fence's marker: %v", err)
		}
		return errFenced
	})
	if err := db.Write(ctx, func(q store.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO notes (id) VALUES (?)`, "w1")
		return err
	}); !errors.Is(err, errFenced) {
		t.Errorf("Write: %v, want the fence's error", err)
	}
	if err := inSQLTx(ctx, db, func(tx *sql.Tx) error {
		if err := db.Fence(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO notes (id) VALUES (?)`, "w2")
		return err
	}); !errors.Is(err, errFenced) {
		t.Errorf("Fence: %v, want the fence's error", err)
	}
	if calls != 2 {
		t.Errorf("the fence ran %d times, want once for each write", calls)
	}
	store.SetFenceForTest(db, nil)
	if n := count(t, db, `SELECT COUNT(*) FROM notes`); n != 0 {
		t.Fatalf("the refused writes left %d rows: the fence ran outside their transactions", n)
	}
}

func migrationLockRunsTheMigration(t *testing.T, noun string) {
	db := open(t, filepath.Join(t.TempDir(), "addon.db"), noun)
	ran := false
	if err := db.WithMigrationLock(context.Background(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("WithMigrationLock = %v, ran %v", err, ran)
	}
	want := errors.New("the migration's error")
	if err := db.WithMigrationLock(context.Background(), func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("WithMigrationLock = %v, want the migration's error", err)
	}
}
