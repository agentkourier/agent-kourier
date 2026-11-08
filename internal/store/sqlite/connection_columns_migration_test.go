package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestMigration0029RowsWrittenBeforeReadAsNoConnectionAndAreAttributed: an outbox row and a press written by the schema of
// 0028, which has no connection column, read as having none after the migration, are attributed to the connection AttributeConnection
// is given, and a second call changes nothing.
func TestMigration0029RowsWrittenBeforeReadAsNoConnectionAndAreAttributed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	db, err := openPool(ctx, path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrationsThrough(t, 28)); err != nil {
		_ = db.Close()
		t.Fatalf("migrate through 0028: %v", err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, payload) VALUES ('old-unposted', 'e1', 'reply', 'C1', '1.1', %d, '{"text":"hi"}')`, at.UnixNano()),
		fmt.Sprintf(`INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, message_ref) VALUES ('old-posted', 'e2', 'reply', 'C1', '1.1', %d, '9.9')`, at.UnixNano()),
		fmt.Sprintf(`INSERT INTO interaction_presses (interaction_id, user_id, channel, message_ts, inputs, pressed_at) VALUES ('ix-1', 'U1', 'C1', '1.2', x'7b7d', %d)`, at.UnixNano()),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			_ = db.Close()
			t.Fatalf("insert a row of the old schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
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
	if o, err := s.GetOutbox(ctx, "old-unposted"); err != nil || o.Connection != "ns/own" {
		t.Errorf("GetOutbox(old-unposted) after a second call for another connection = %+v, %v; want ns/own", o, err)
	}
}

// TestOutboxThreadIndexStartsWithTheConnection: the purge of a session's outbox rows, the check that none is pending and
// AttributeConnection read the outbox by connection first.
func TestOutboxThreadIndexStartsWithTheConnection(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "agent-kourier.db"))
	rows, err := s.a.read.QueryContext(t.Context(), `SELECT name FROM pragma_index_info('outbox_thread') ORDER BY seqno`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
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
	if fmt.Sprint(cols) != "[connection channel thread_ts]" {
		t.Errorf("outbox_thread is on %v; want [connection channel thread_ts]", cols)
	}
}

// TestOutboxQueriesByConnectionUseTheThreadIndex: the purge of a thread's outbox rows and AttributeConnection's update search
// outbox_thread, and never read the whole table, which holds a row for every post until its session is deleted.
func TestOutboxQueriesByConnectionUseTheThreadIndex(t *testing.T) {
	s := openTemp(t)
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"the purge of a thread":  {`DELETE FROM outbox WHERE connection = ? AND channel = ? AND thread_ts = ?`, []any{"c", "C1", "1.1"}},
		"the attribution update": {`UPDATE outbox SET connection = ? WHERE connection = ''`, []any{"c"}},
	} {
		plan := strings.Join(queryPlan(t, s, q.sql, q.args), "\n")
		if !strings.Contains(plan, "USING INDEX outbox_thread") && !strings.Contains(plan, "USING COVERING INDEX outbox_thread") {
			t.Errorf("%s does not use outbox_thread: %q", name, plan)
		}
	}
}

// TestServedSinceIsKeptForAConnectionThatStaysAndSetForANewOne: a replace that keeps a connection leaves the time it has been served
// since, and the connections it adds are served from the time of the replace.
func TestServedSinceIsKeptForAConnectionThatStaysAndSetForANewOne(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "agent-kourier.db"))
	ctx := t.Context()
	since := func() map[string]int64 {
		t.Helper()
		rows, err := s.a.read.QueryContext(ctx, `SELECT connection, served_since FROM served_connections`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]int64{}
		for rows.Next() {
			var c string
			var at int64
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
	if err := s.ReplaceServedConnections(ctx, []string{"ns/stays", "ns/goes"}); err != nil {
		t.Fatal(err)
	}
	first := since()
	if first["ns/stays"] <= 0 {
		t.Fatalf("served_since = %v; want a time for each connection", first)
	}
	time.Sleep(5 * time.Millisecond)
	if err := s.ReplaceServedConnections(ctx, []string{"ns/stays", "ns/new"}); err != nil {
		t.Fatal(err)
	}
	second := since()
	if len(second) != 2 || second["ns/stays"] != first["ns/stays"] || second["ns/new"] <= first["ns/stays"] {
		t.Errorf("served_since was %v, is now %v; want ns/stays unchanged, ns/goes gone, and ns/new later than ns/stays", first, second)
	}
}
