package store

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNumbered(t *testing.T) {
	for in, want := range map[string]string{
		`SELECT 1`:                          `SELECT 1`,
		`SELECT ? FROM t WHERE a = ? AND ?`: `SELECT $1 FROM t WHERE a = $2 AND $3`,
		`INSERT INTO t VALUES (?, 'é', ?)`:  `INSERT INTO t VALUES ($1, 'é', $2)`,
	} {
		if got := numbered(in); got != want {
			t.Errorf("numbered(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListMigrationsRefusesBadNames(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		files []string
		want  string
	}{
		{[]string{"notes.sql"}, "is not named NNNN_name.sql"},
		{[]string{"0000_zero.sql"}, "is not named NNNN_name.sql"},
		{[]string{"0001_notes.txt"}, "is not named NNNN_name.sql"},
		{[]string{"0001_a.sql", "1_b.sql"}, "share version 1"},
	} {
		sub := filepath.Join(dir, strings.Join(tc.files, "+"))
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range tc.files {
			if err := os.WriteFile(filepath.Join(sub, f), []byte("SELECT 1;"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := listMigrations(os.DirFS(sub)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("listMigrations(%v) = %v, want %q", tc.files, err, tc.want)
		}
	}
}

func kitSub(t *testing.T, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(kitMigrationFiles, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// The two backends' kit migrations are one list: the same files, translated.
func TestKitMigrationsInStep(t *testing.T) {
	sq, err := listMigrations(kitSub(t, "migrations/sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	pg, err := listMigrations(kitSub(t, "migrations/postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sq) == 0 || len(sq) != len(pg) {
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

// fakeConn runs every transaction with a nil Querier: enough for a write that does not query.
type fakeConn struct{ conn }

func (fakeConn) inTx(_ context.Context, fn func(q Querier) error) error { return fn(nil) }

func (fakeConn) over(SQLDB) Querier { return nil }

// SetFenceForTest may replace the fence while writes run: the race detector sees no unsynchronised access, and each
// write runs one fence or none.
func TestSetFenceForTestWhileWritesRun(t *testing.T) {
	db := newDB(fakeConn{}, Options{})
	var calls atomic.Int64
	fence := func(context.Context, Querier) error { calls.Add(1); return nil }
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				if err := db.Write(context.Background(), func(Querier) error { return nil }); err != nil {
					t.Errorf("Write: %v", err)
				}
				if err := db.Fence(context.Background(), nil); err != nil {
					t.Errorf("Fence: %v", err)
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 200 {
			if i%2 == 0 {
				SetFenceForTest(db, fence)
			} else {
				SetFenceForTest(db, nil)
			}
		}
	})
	wg.Wait()
	SetFenceForTest(db, fence)
	before := calls.Load()
	if err := db.Write(context.Background(), func(Querier) error { return nil }); err != nil || calls.Load() != before+1 {
		t.Fatalf("a write after SetFenceForTest: %v, fence ran %d times, want once", err, calls.Load()-before)
	}
	SetFenceForTest(db, nil)
	if err := db.Write(context.Background(), func(Querier) error { return nil }); err != nil || calls.Load() != before+1 {
		t.Fatalf("a write after SetFenceForTest(nil): %v, fence ran %d times, want none", err, calls.Load()-before-1)
	}
}
