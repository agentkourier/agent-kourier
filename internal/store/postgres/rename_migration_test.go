package postgres

import (
	"io/fs"
	"log/slog"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// renameMigration is the number of the migration that gave the audit purge and its setting their Agent
// Kourier names; renamedFrom is the last migration of the release before it.
const (
	renameMigration = 23
	renamedFrom     = renameMigration - 1
)

// purgeFunctions lists the audit purge functions of the connection's schema, as name/argument count.
func purgeFunctions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT proname || '/' || pronargs
		FROM pg_proc
		WHERE pronamespace = current_schema()::regnamespace AND proname LIKE '%audit_purge'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("list purge functions: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRenameMigrationUpgradesAnOldDatabase: a database that the release before the rename migrated (it has
// courier_audit_purge and the courier.audit_retention setting) comes out of Open with the Agent Kourier
// names beside the old ones, and both purge. A rolling update runs the old binary next to the new one and a
// rollback brings it back, so the old function must keep working until a later migration drops it; and the
// audit table stays as strict as before. Applied migrations are not edited, so this is the path every
// existing database takes.
func TestRenameMigrationUpgradesAnOldDatabase(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The previous release's schema: the migrations up to the one before the rename.
	all, err := listMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, m := range all {
		if m.version > renamedFrom {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, m.name)
		if err != nil {
			t.Fatal(err)
		}
		previous[m.name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(ctx, pool, previous, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate to the previous release: %v", err)
	}
	if got, want := purgeFunctions(t, pool), []string{"courier_audit_purge/1", "courier_audit_purge/2"}; !slices.Equal(got, want) {
		t.Fatalf("before the rename the purge functions are %v; want %v", got, want)
	}

	// Three entries the old binary wrote: two the purges below remove, one they keep.
	oldest, middle, recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{oldest, middle, recent} {
		if _, err := pool.Exec(ctx, `INSERT INTO audit (at, kind, subject, outcome) VALUES ($1, 'event', 'before-rename', 'accepted')`, at); err != nil {
			t.Fatalf("insert an audit entry as the previous release: %v", err)
		}
	}

	// The new binary opens the same database: 0023 is the only migration left to apply.
	s := openAt(t, dsn)
	want := []string{"agentkourier_audit_purge/1", "agentkourier_audit_purge/2", "courier_audit_purge/1", "courier_audit_purge/2"}
	if got := purgeFunctions(t, pool); !slices.Equal(got, want) {
		t.Fatalf("after the rename the purge functions are %v; want the new pair beside the old: %v", got, want)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit`); !refused(err) {
		t.Errorf("delete outside a purge = %v; want the trigger's refusal", err)
	}

	// The old binary's purge, still called by a replica that has not been replaced: it removes the oldest entry.
	var purged int64
	if err := pool.QueryRow(ctx, `SELECT courier_audit_purge($1)`, oldest.Add(24*time.Hour)).Scan(&purged); err != nil || purged != 1 {
		t.Fatalf("courier_audit_purge after 0023 = %d, %v; want the entry before the cutoff", purged, err)
	}

	// The new binary's purge, through the store: it removes the middle one and keeps the recent one.
	n, err := s.PurgeAudit(ctx, middle.Add(24*time.Hour), 10)
	if err != nil || n != 1 {
		t.Fatalf("PurgeAudit = %d, %v; want the one entry before the cutoff", n, err)
	}
	if got := countAudit(t, s); got != 1 {
		t.Errorf("%d entries after both purges; want the recent one", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit`); !refused(err) {
		t.Errorf("delete after the purges = %v; want the trigger's refusal: neither purge may leave the table open", err)
	}
}
