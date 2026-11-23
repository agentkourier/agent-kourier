package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

// testLockKey is the tests' migration lock, as an add-on's MigrationLockKey.
const testLockKey int64 = 0x6b69745f74657374 // "kit_test"

// openPostgresWith opens a database on dsn, as one replica would, with opts and the notes migration applied.
func openPostgresWith(t *testing.T, dsn string, opts Options) *DB {
	t.Helper()
	opts.MigrationLockKey = testLockKey
	db, err := OpenPostgres(context.Background(), dsn, opts)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background(), os.DirFS("testdata/migrations")); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func openPostgres(t *testing.T, dsn string) *DB { return openPostgresWith(t, dsn, Options{}) }

func count(t *testing.T, db *DB, query string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// Replicas that start together on an empty database each apply the migrations, the kit's and the add-on's, once
// between them.
func TestPostgresReplicasMigrateTogether(t *testing.T) {
	dsn := pgtest.DSN(t)
	const replicas = 4
	var wg sync.WaitGroup
	for range replicas {
		wg.Go(func() {
			db, err := OpenPostgres(context.Background(), dsn, Options{MigrationLockKey: testLockKey})
			if err != nil {
				t.Errorf("OpenPostgres: %v", err)
				return
			}
			defer func() { _ = db.Close() }()
			if err := db.Migrate(context.Background(), os.DirFS("testdata/migrations")); err != nil {
				t.Errorf("Migrate: %v", err)
			}
		})
	}
	wg.Wait()
	db := openPostgres(t, dsn)
	ms, err := listMigrations(kitSub(t, "migrations/postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM kit_schema_migrations`); n != len(ms) {
		t.Fatalf("kit_schema_migrations has %d rows, want %d", n, len(ms))
	}
	if n := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema_migrations has %d rows, want 1", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM leadership`); n != 1 {
		t.Fatalf("leadership has %d rows, want 1", n)
	}
}

func TestPostgresErrorsHideTheDSN(t *testing.T) {
	const secret = "fake-password-do-not-print"
	for _, dsn := range []string{"postgres://u:" + secret + "@[::1:5432/db", "host=127.0.0.1 port=1 user=u password=" + secret + " connect_timeout=1"} {
		_, err := OpenPostgres(context.Background(), dsn, Options{MigrationLockKey: testLockKey})
		if err == nil {
			t.Fatal("OpenPostgres accepted it")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error shows the password: %v", err)
		}
	}
}

// A database an add-on created before the kit has the leadership table from the add-on's own migration 0002, and a
// lease in it. The kit's migration adopts the table: the row, the lease and the table itself stay, the kit records its
// migration once, and the add-on's version table is the add-on's.
func TestPostgresAdoptsTheAddOnsLeadershipTable(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ddl, err := os.ReadFile("testdata/addon-0002-leadership.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version integer PRIMARY KEY, applied_at bigint NOT NULL)`,
		string(ddl),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (2, 0)`,
		`UPDATE leadership SET holder = 'old-pod', address = 'http://old-pod:8090', epoch = 7, expires_at = now() + interval '1 hour'`,
	} {
		if _, err := raw.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var oid uint32
	if err := raw.QueryRow(ctx, `SELECT 'leadership'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	kit, err := listMigrations(kitSub(t, "migrations/postgres"))
	if err != nil {
		t.Fatal(err)
	}

	for range 2 { // the second open finds the migrations recorded
		db, err := OpenPostgres(ctx, dsn, Options{MigrationLockKey: testLockKey})
		if err != nil {
			t.Fatalf("OpenPostgres: %v", err)
		}
		lease, err := db.CurrentLeader(ctx)
		if err != nil || lease != (Lease{Holder: "old-pod", Address: "http://old-pod:8090", Epoch: 7, Live: true}) {
			t.Fatalf("the lease after the kit's migration: %+v, %v", lease, err)
		}
		if n := count(t, db, `SELECT COUNT(*) FROM kit_schema_migrations`); n != len(kit) {
			t.Fatalf("kit_schema_migrations has %d rows, want the kit's %d", n, len(kit))
		}
		if n := count(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version <> 2`); n != 0 {
			t.Fatalf("the kit wrote %d rows into the add-on's schema_migrations", n)
		}
		_ = db.Close()
	}
	var after uint32
	if err := raw.QueryRow(ctx, `SELECT 'leadership'::regclass::oid`).Scan(&after); err != nil || after != oid {
		t.Fatalf("the leadership table is %d (%v), was %d: it was recreated", after, err, oid)
	}
}

// Prepare runs before the kit's migrations, in a transaction that commits its writes, and its error fails the open
// before any migration has run.
func TestPostgresPrepareRunsBeforeTheMigrations(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	refuse := errors.New("an earlier build's layout")
	_, err := OpenPostgres(ctx, dsn, Options{MigrationLockKey: testLockKey, Prepare: func(ctx context.Context, q Querier) error {
		if _, err := q.Exec(ctx, `CREATE TABLE prepared (id text PRIMARY KEY)`); err != nil {
			return err
		}
		return refuse
	}})
	if !errors.Is(err, refuse) {
		t.Fatalf("an open whose Prepare refuses: %v, want its error", err)
	}
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var tables int
	if err := raw.QueryRow(ctx, `SELECT COUNT(*) FROM pg_tables WHERE schemaname = current_schema()`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("the refused open left %d tables (%v), want none: no migration, and Prepare's write rolled back", tables, err)
	}

	var kitTable *string
	db := openPostgresWith(t, dsn, Options{Prepare: func(ctx context.Context, q Querier) error {
		if err := q.QueryRow(ctx, `SELECT to_regclass('kit_schema_migrations')::text`).Scan(&kitTable); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `CREATE TABLE prepared (id text PRIMARY KEY)`)
		return err
	}})
	if kitTable != nil {
		t.Fatalf("Prepare found %s: it ran after the kit's migrations", *kitTable)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename IN ('prepared', 'kit_schema_migrations')`); n != 2 {
		t.Fatalf("after the open: %d of Prepare's table and the kit's version table, want both", n)
	}
}

// Replicas that start together run Prepare one at a time: it holds the migration lock.
func TestPostgresPrepareHoldsTheMigrationLock(t *testing.T) {
	dsn := pgtest.DSN(t)
	var mu sync.Mutex
	inside, most := 0, 0
	prepare := func(context.Context, Querier) error {
		mu.Lock()
		inside++
		most = max(most, inside)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			db, err := OpenPostgres(context.Background(), dsn, Options{MigrationLockKey: testLockKey, Prepare: prepare})
			if err != nil {
				t.Errorf("OpenPostgres: %v", err)
				return
			}
			_ = db.Close()
		})
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d Prepares ran at once, want 1", most)
	}
}

// A library's migration under WithMigrationLock runs alone, however many replicas start together.
func TestPostgresMigrationLockSerializes(t *testing.T) {
	dsn := pgtest.DSN(t)
	var mu sync.Mutex
	inside, most := 0, 0
	var wg sync.WaitGroup
	for range 3 {
		db := openPostgres(t, dsn)
		wg.Go(func() {
			err := db.WithMigrationLock(context.Background(), func() error {
				mu.Lock()
				inside++
				most = max(most, inside)
				mu.Unlock()
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("WithMigrationLock: %v", err)
			}
		})
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d migrations ran at once under the lock, want 1", most)
	}
}
