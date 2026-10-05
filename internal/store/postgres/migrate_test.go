package postgres

import (
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func migrationNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestMigrationsInStepWithSQLite: the Postgres migrations have the numbers and names of the SQLite
// store's, one for one. A migration that only SQLite needs is an empty file here, with a comment that
// says why; a new SQLite migration has to be answered with one.
func TestMigrationsInStepWithSQLite(t *testing.T) {
	got, want := migrationNames(t, "migrations"), migrationNames(t, "../sqlite/migrations")
	if !slices.Equal(got, want) {
		t.Errorf("postgres migrations = %v; want those of the SQLite store, %v: add the twin of the migration that has none", got, want)
	}
}

func TestListMigrations(t *testing.T) {
	ms, err := listMigrations(fstest.MapFS{
		"migrations/0010_ten.sql": {}, "migrations/0002_two.sql": {}, "migrations/0001_one.sql": {},
	})
	if err != nil || len(ms) != 3 || ms[0].version != 1 || ms[1].version != 2 || ms[2].version != 10 {
		t.Errorf("listMigrations = %+v, %v; want 1, 2, 10 in numeric order", ms, err)
	}
	if _, err := listMigrations(fstest.MapFS{"migrations/0001_a.sql": {}, "migrations/1_b.sql": {}}); err == nil {
		t.Error("two files with one version were accepted")
	}
	if _, err := listMigrations(fstest.MapFS{"migrations/x_a.sql": {}}); err == nil {
		t.Error("a file with no numeric version was accepted")
	}
}

func appliedVersions(t *testing.T, dsn string) map[int]time.Time {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rows, err := pool.Query(t.Context(), `SELECT version, applied_at FROM schema_migrations`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	got := map[int]time.Time{}
	for rows.Next() {
		var v int
		var at time.Time
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		got[v] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestMigrateIsIdempotent: Open on a database that is migrated applies nothing again.
func TestMigrateIsIdempotent(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	openAt(t, dsn)
	first := appliedVersions(t, dsn)
	if want := len(migrationNames(t, "migrations")); len(first) != want {
		t.Fatalf("%d versions recorded after the first Open; want %d", len(first), want)
	}
	openAt(t, dsn)
	second := appliedVersions(t, dsn)
	if len(second) != len(first) {
		t.Fatalf("%d versions after the second Open; want %d", len(second), len(first))
	}
	for v, at := range first {
		if !second[v].Equal(at) {
			t.Errorf("migration %d was applied again: applied_at %v, then %v", v, at, second[v])
		}
	}
}

// TestConcurrentOpenMigratesOnce: replicas that start together, on an empty database, all come up with
// the schema made once. Without the advisory lock two of them run CREATE TABLE for one table, and one
// fails.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	const replicas = 8
	stores := make([]*Store, replicas)
	errs := make([]error, replicas)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() {
			<-start
			stores[i], errs[i] = Open(t.Context(), dsn, WithMaxConns(2))
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: Open: %v", i, err)
			continue
		}
		t.Cleanup(func() { _ = stores[i].Close() })
	}
	if got, want := len(appliedVersions(t, dsn)), len(migrationNames(t, "migrations")); got != want {
		t.Errorf("%d migrations recorded; want %d, each once", got, want)
	}
}

// TestMigrationFailureLeavesEarlierOnes: a migration that fails is not applied at all, and the ones
// before it stay applied.
func TestMigrationFailureLeavesEarlierOnes(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fsys := fstest.MapFS{
		"migrations/0001_ok.sql":    {Data: []byte(`CREATE TABLE ok (id integer);`)},
		"migrations/0002_bad.sql":   {Data: []byte(`CREATE TABLE half (id integer); SELECT 1/0;`)},
		"migrations/0003_never.sql": {Data: []byte(`CREATE TABLE never (id integer);`)},
	}
	if err := migrate(t.Context(), pool, fsys, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("migrate = %v; want the failure of 0002_bad.sql", err)
	}
	got := appliedVersions(t, dsn)
	if len(got) != 1 || got[1].IsZero() {
		t.Errorf("applied = %v; want only migration 1", got)
	}
	for table, want := range map[string]bool{"ok": true, "half": false, "never": false} {
		var exists bool
		if err := pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != want {
			t.Errorf("table %s exists = %v; want %v", table, exists, want)
		}
	}
}
