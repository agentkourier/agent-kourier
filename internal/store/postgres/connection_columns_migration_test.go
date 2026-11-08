package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestMigration0029RowsWrittenBeforeReadAsNoConnectionAndAreAttributed: an outbox row and a press written by the schema of
// 0028, which has no connection column, read as having none after the migration, are attributed to the connection
// AttributeConnection is given, and a second call changes nothing.
func TestMigration0029RowsWrittenBeforeReadAsNoConnectionAndAreAttributed(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	migrateUpTo(t, p, 28)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, payload) VALUES ('old-unposted', 'e1', 'reply', 'C1', '1.1', $1, '{"text":"hi"}')`,
		`INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, message_ref) VALUES ('old-posted', 'e2', 'reply', 'C1', '1.1', $1, '9.9')`,
		`INSERT INTO interaction_presses (interaction_id, user_id, channel, message_ts, inputs, pressed_at) VALUES ('ix-1', 'U1', 'C1', '1.2', '\x7b7d', $1)`,
	} {
		if _, err := p.Exec(ctx, q, at); err != nil {
			p.Close()
			t.Fatalf("insert a row of the old schema: %v", err)
		}
	}
	p.Close()

	s := openAt(t, dsn)
	for _, id := range []string{"old-unposted", "old-posted"} {
		if o, err := s.GetOutbox(ctx, id); err != nil || o.Connection != "" || o.Channel != "C1" || o.Thread != "1.1" {
			t.Fatalf("GetOutbox(%s) = %+v, %v; want the row with no connection", id, o, err)
		}
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 1 || l[0].ID != "old-unposted" || l[0].Connection != "" {
		t.Fatalf("ListUnposted = %+v, %v; want the unposted row with no connection", l, err)
	}
	if l, err := s.ListPresses(ctx); err != nil || len(l) != 1 || l[0].Connection != "" || l[0].InteractionID != "ix-1" {
		t.Fatalf("ListPresses = %+v, %v; want the press with no connection", l, err)
	}

	got, err := s.AttributeConnection(ctx, "ns/own")
	if err != nil || got != (store.AttributedRows{Outbox: 2, Presses: 1}) {
		t.Fatalf("AttributeConnection = %+v, %v; want 2 outbox rows and 1 press", got, err)
	}
	if o, err := s.GetOutbox(ctx, "old-posted"); err != nil || o.Connection != "ns/own" {
		t.Errorf("GetOutbox(old-posted) = %+v, %v; want connection ns/own", o, err)
	}
	if l, err := s.ListPresses(ctx); err != nil || len(l) != 1 || l[0].Connection != "ns/own" {
		t.Errorf("ListPresses = %+v, %v; want connection ns/own", l, err)
	}
	if again, err := s.AttributeConnection(ctx, "ns/other"); err != nil || again != (store.AttributedRows{}) {
		t.Errorf("AttributeConnection again = %+v, %v; want nothing: the rows are attributed", again, err)
	}
}

// TestAttributeConnectionIsFenced: a store that is not the leader changes nothing, like every write.
func TestAttributeConnectionIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	standby := openStandbyAt(t, dsn)
	if _, err := standby.AttributeConnection(ctx, "ns/own"); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("AttributeConnection of a standby = %v; want store.ErrDeposed", err)
	}
}

// TestOutboxThreadIndexStartsWithTheConnection: the purge of a session's outbox rows, the check that none is pending and
// AttributeConnection read the outbox by connection first.
func TestOutboxThreadIndexStartsWithTheConnection(t *testing.T) {
	dsn, schema, _ := freshSchema(t)
	s := openAt(t, dsn)
	rows, err := s.a.pool.Query(t.Context(), `SELECT a.attname FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE c.relname = 'outbox_thread' AND n.nspname = $1 ORDER BY k.ord`, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cols) != 3 || cols[0] != "connection" || cols[1] != "channel" || cols[2] != "thread_ts" {
		t.Errorf("outbox_thread is on %v; want [connection channel thread_ts]", cols)
	}
}

// TestServedSinceIsKeptForAConnectionThatStaysAndSetForANewOne: a replace that keeps a connection leaves the time it has been served
// since, and the connections it adds are served from the time of the replace.
func TestServedSinceIsKeptForAConnectionThatStaysAndSetForANewOne(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	s := openAt(t, dsn)
	ctx := t.Context()
	since := func() map[string]time.Time {
		t.Helper()
		rows, err := s.a.pool.Query(ctx, `SELECT connection, served_since FROM served_connections`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]time.Time{}
		for rows.Next() {
			var c string
			var at time.Time
			if err := rows.Scan(&c, &at); err != nil {
				t.Fatal(err)
			}
			out[c] = at
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := s.ReplaceServedConnections(ctx, []string{"ns/stays", "ns/goes"}, at); err != nil {
		t.Fatal(err)
	}
	first := since()
	if !first["ns/stays"].Equal(at) || !first["ns/goes"].Equal(at) {
		t.Fatalf("served_since = %v; want the time of the call for each connection, %v", first, at)
	}
	if err := s.ReplaceServedConnections(ctx, []string{"ns/stays", "ns/new"}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	second := since()
	if len(second) != 2 || !second["ns/stays"].Equal(at) || !second["ns/new"].Equal(at.Add(time.Hour)) {
		t.Errorf("served_since was %v, is now %v; want ns/stays to keep %v, ns/goes gone, and ns/new served from %v", first, second, at, at.Add(time.Hour))
	}
}

// TestReplaceServedConnectionsIsFenced: a store that is not the leader changes nothing, like every write.
func TestReplaceServedConnectionsIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	standby := openStandbyAt(t, dsn)
	if err := standby.ReplaceServedConnections(t.Context(), []string{"ns/a"}, time.Now()); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("ReplaceServedConnections of a standby = %v; want store.ErrDeposed", err)
	}
	if got, err := standby.ServedConnections(t.Context()); err != nil || len(got) != 0 {
		t.Errorf("ServedConnections = %v, %v; want none: the standby's write changed nothing", got, err)
	}
}

// TestAttributeConnectionAttributesTablesLargerThanOneBatch: rows without a connection in more than one batch (the outbox, and
// the presses) are all attributed, the count is of all of them, and rows that have a connection are left alone.
func TestAttributeConnectionAttributesTablesLargerThanOneBatch(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	s := openAt(t, dsn)
	ctx := t.Context()
	n := 2*store.AttributeBatch + 1
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at) SELECT 'o' || i, 'e' || i, 'reply', 'C1', '1.1', $2 FROM generate_series(1, $1::int) i`,
		`INSERT INTO interaction_presses (interaction_id, user_id, channel, message_ts, inputs, pressed_at) SELECT 'ix', 'U' || i, 'C1', '1.2', '\x7b7d', $2 FROM generate_series(1, $1::int) i`,
	} {
		if _, err := s.a.pool.Exec(ctx, q, n, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.a.pool.Exec(ctx, `UPDATE outbox SET connection = 'ns/other' WHERE id = 'o1'`); err != nil {
		t.Fatal(err)
	}

	got, err := s.AttributeConnection(ctx, "ns/own")
	if want := (store.AttributedRows{Outbox: n - 1, Presses: n}); err != nil || got != want {
		t.Fatalf("AttributeConnection = %+v, %v; want %+v", got, err, want)
	}
	var empty, own, other int
	if err := s.a.pool.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM outbox WHERE connection = '') + (SELECT COUNT(*) FROM interaction_presses WHERE connection = ''),
		(SELECT COUNT(*) FROM outbox WHERE connection = 'ns/own') + (SELECT COUNT(*) FROM interaction_presses WHERE connection = 'ns/own'),
		(SELECT COUNT(*) FROM outbox WHERE connection = 'ns/other')`).Scan(&empty, &own, &other); err != nil {
		t.Fatal(err)
	}
	if empty != 0 || own != 2*n-1 || other != 1 {
		t.Errorf("rows with no connection %d, the own connection's %d, another's %d; want 0, %d and 1", empty, own, other, 2*n-1)
	}
}
