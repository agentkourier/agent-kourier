package postgres

import (
	"context"
	"io/fs"
	"log/slog"
	"net/url"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/internal/session"
)

// The migrations of the rename: 0023 added the Agent Kourier names beside the courier ones, and 0024 drops the
// courier ones and renames the stored system user.
const (
	addedNames   = 23
	droppedNames = addedNames + 1
)

// oldSystemUser is the value of session.SystemUser before the rename. The value after it is session.SystemUser
// itself, so that a constant changed without a migration, or a migration written for another value, fails the test.
const oldSystemUser = "courier:system"

// purgeFunctions lists the audit purge functions of the connection's schema, as name/argument count.
func purgeFunctions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT proname || '/' || pronargs
		FROM pg_proc
		WHERE pronamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema()) AND proname LIKE '%audit_purge'
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

// migrateUpTo applies the migrations numbered up to last to the schema of pool.
func migrateUpTo(t *testing.T, pool *pgxpool.Pool, last int) {
	t.Helper()
	all, err := listMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	part := fstest.MapFS{}
	for _, m := range all {
		if m.version > last {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, m.name)
		if err != nil {
			t.Fatal(err)
		}
		part[m.name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(t.Context(), pool, part, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate up to %d: %v", last, err)
	}
}

// deleteUnderSetting deletes every audit row in a transaction that has set the named retention setting on, and
// returns the error, if any; the transaction is rolled back, so a delete that went through changes nothing.
func deleteUnderSetting(t *testing.T, pool *pgxpool.Pool, setting string) error {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config($1, 'on', true)`, setting); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(t.Context(), `DELETE FROM audit`)
	return err
}

// mixedCaseSchema is freshSchema for a schema whose name must be quoted: it has capitals, so the DSN's search_path
// names it in double quotes, as a deployment that keeps the schema as AgentKourier would.
func mixedCaseSchema(t *testing.T) string {
	t.Helper()
	base := serverDSN(t)
	schema := "AgentKourier_" + randHex(t)
	if _, err := admin(t).Exec(t.Context(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("connect to drop the schema: %v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("the test server's DSN must be a URL") // the error would quote the DSN
	}
	q := u.Query()
	q.Set("search_path", `"`+schema+`"`)
	q.Set("application_name", "agent-kourier-test-"+randHex(t))
	u.RawQuery = q.Encode()
	return u.String()
}

// TestDropCourierNamesMigrationUpgradesAnOldDatabase: a database at 0023, the last migration that kept the old
// project name beside the new one, comes out of Open without it. The courier_audit_purge functions and the
// courier.audit_retention setting are gone, the stored system user and the auto-deny decider are the new ones,
// while a person's turns and decisions, the audit rows and the new purge are as before. Applied migrations are not
// edited, so this is the path every existing database takes. It runs in a schema with a lower-case name and in one
// that must be quoted.
func TestDropCourierNamesMigrationUpgradesAnOldDatabase(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		dsn, _, _ := freshSchema(t)
		testDropCourierNames(t, dsn)
	})
	t.Run("mixed-case schema", func(t *testing.T) {
		testDropCourierNames(t, mixedCaseSchema(t))
	})
}

func testDropCourierNames(t *testing.T, dsn string) {
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	migrateUpTo(t, pool, addedNames)
	before := []string{"agentkourier_audit_purge/1", "agentkourier_audit_purge/2", "courier_audit_purge/1", "courier_audit_purge/2"}
	if got := purgeFunctions(t, pool); !slices.Equal(got, before) {
		t.Fatalf("at 0023 the purge functions are %v; want the new pair beside the old: %v", got, before)
	}

	// Three audit entries, two for the purges below to remove and one to keep, and the rows a system turn and a
	// person's turn leave in the reply queue and the session: what the dev pre-releases wrote.
	oldest, middle, recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{oldest, middle, recent} {
		if _, err := pool.Exec(ctx, `INSERT INTO audit (at, kind, subject, outcome) VALUES ($1, 'event', 'before-drop', 'accepted')`, at); err != nil {
			t.Fatalf("insert an audit entry: %v", err)
		}
	}
	for id, user := range map[string]string{"system-turn": oldSystemUser, "person-turn": "U1"} {
		if _, err := pool.Exec(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, text, enqueued_at)
			VALUES ($1, 'slack', 'C1', $1, $2, '1.1', 'hello', now())`, id, user); err != nil {
			t.Fatalf("insert a reply: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (connection, channel, thread_ts, context_id, task_id, binding, last_activity, turn_id, turn_user)
			VALUES ('slack', 'C1', $1, 'ctx', 'task', 'sre/sre-alerts', now(), 'turn-1', $2)`, id, user); err != nil {
			t.Fatalf("insert a session: %v", err)
		}
	}

	// An approval the auto-deny recorded under the decider's old name, and one a person decided.
	for id, decider := range map[string]string{"auto-denied": "courier", "person-decided": "U1"} {
		if _, err := pool.Exec(ctx, `INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, state, request, created_at, deadline, decider, decided_at)
			VALUES ($1, 'slack', 'C1', 'ix', 'task', 'tool_approval', 'answered', '{}', now(), now() + interval '1 hour', $2, now())`, id, decider); err != nil {
			t.Fatalf("insert an interaction: %v", err)
		}
	}

	// At 0023 the old setting still lets a delete through, and the old function purges.
	if err := deleteUnderSetting(t, pool, "courier.audit_retention"); err != nil {
		t.Fatalf("at 0023 a delete under courier.audit_retention = %v; want it let through", err)
	}
	var purged int64
	if err := pool.QueryRow(ctx, `SELECT courier_audit_purge($1)`, oldest.Add(24*time.Hour)).Scan(&purged); err != nil || purged != 1 {
		t.Fatalf("courier_audit_purge at 0023 = %d, %v; want the entry before the cutoff", purged, err)
	}

	// The new binary opens the same database: 0024 is the only migration left to apply.
	s := openAt(t, dsn)
	if got, want := purgeFunctions(t, pool), []string{"agentkourier_audit_purge/1", "agentkourier_audit_purge/2"}; !slices.Equal(got, want) {
		t.Fatalf("after 0024 the purge functions are %v; want only the new pair: %v", got, want)
	}
	if _, err := pool.Exec(ctx, `SELECT courier_audit_purge($1)`, middle); err == nil {
		t.Error("courier_audit_purge after 0024 ran; want it dropped")
	}
	if err := deleteUnderSetting(t, pool, "courier.audit_retention"); !refused(err) {
		t.Errorf("a delete under courier.audit_retention after 0024 = %v; want the trigger's refusal", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit`); !refused(err) {
		t.Errorf("delete outside a purge = %v; want the trigger's refusal", err)
	}

	// The stored system user is the new one, and a person's is untouched.
	for _, tc := range []struct{ query, id, want string }{
		{`SELECT user_id FROM reply_queue WHERE id = $1`, "system-turn", session.SystemUser},
		{`SELECT user_id FROM reply_queue WHERE id = $1`, "person-turn", "U1"},
		{`SELECT turn_user FROM sessions WHERE thread_ts = $1`, "system-turn", session.SystemUser},
		{`SELECT turn_user FROM sessions WHERE thread_ts = $1`, "person-turn", "U1"},
		{`SELECT decider FROM interactions WHERE id = $1`, "auto-denied", "agent-kourier"},
		{`SELECT decider FROM interactions WHERE id = $1`, "person-decided", "U1"},
	} {
		var got string
		if err := pool.QueryRow(ctx, tc.query, tc.id).Scan(&got); err != nil || got != tc.want {
			t.Errorf("%s for %s = %q, %v; want %q", tc.query, tc.id, got, err, tc.want)
		}
	}

	// The new binary's purge, through the store, with the new setting: it removes the middle entry and keeps the recent one.
	n, err := s.PurgeAudit(ctx, middle.Add(24*time.Hour), 10)
	if err != nil || n != 1 {
		t.Fatalf("PurgeAudit = %d, %v; want the one entry before the cutoff", n, err)
	}
	if got := countAudit(t, s); got != 1 {
		t.Errorf("%d entries after the purge; want the recent one", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit`); !refused(err) {
		t.Errorf("delete after the purge = %v; want the trigger's refusal: the purge may not leave the table open", err)
	}
}
