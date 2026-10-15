package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func migrationsThrough(t *testing.T, last int) fstest.MapFS {
	t.Helper()
	ms, err := listMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{}
	for _, m := range ms {
		if m.version > last {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, m.name)
		if err != nil {
			t.Fatal(err)
		}
		fsys[m.name] = &fstest.MapFile{Data: body}
	}
	return fsys
}

func TestMigration0026MovesLegacySessionAlertIntoOrigin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	db, err := openPool(ctx, path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrationsThrough(t, 25)); err != nil {
		_ = db.Close()
		t.Fatalf("migrate through 0025: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO sessions
		(connection, channel, thread_ts, context_id, task_id, turn_id, turn_user, turn_task, turn_message_ts,
		 turn_acknowledgment, turn_acknowledgment_id, binding, alert_name, agent_name, last_activity, trace_parent, trace_state)
		VALUES ('slack', 'C1', '1.1', 'ctx', 'task', 'turn', 'U1', 'task', '1.2', 'eyes', 'ack', 'ns/b',
		 'KubePodCrashLooping', 'kagent/sre', 123456789, 'parent', 'state')`)
	if err != nil {
		_ = db.Close()
		t.Fatalf("insert legacy session: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO feedback
		(connection, channel, thread_ts, turn_id, user_id, binding, agent, alert_name, kind, verdict, first_at, voted_at)
		VALUES ('slack', 'C1', '1.1', 'turn', 'U1', 'ns/b', 'kagent/sre', 'KubePodCrashLooping', 'investigation', 'useful', 1, 2)`)
	if err != nil {
		_ = db.Close()
		t.Fatalf("insert feedback row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	var columns []string
	rows, err := s.a.read.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(columns, ","), "alert_name") || !containsAll(columns, "origin_source", "origin_subject") {
		t.Fatalf("sessions columns = %v; want origin columns and no alert_name", columns)
	}

	var origin store.Origin
	var gotContext, gotTask, gotTurn, gotUser, gotTurnTask, gotMessage, gotAck, gotAckID, binding, agent, parent, state string
	var last int64
	if err := s.a.read.QueryRowContext(ctx, `SELECT origin_source, origin_subject, context_id, task_id, turn_id, turn_user,
		turn_task, turn_message_ts, turn_acknowledgment, turn_acknowledgment_id, binding, agent_name, last_activity,
		trace_parent, trace_state FROM sessions WHERE connection = 'slack' AND channel = 'C1' AND thread_ts = '1.1'`).Scan(
		&origin.Source, &origin.Subject, &gotContext, &gotTask, &gotTurn, &gotUser, &gotTurnTask, &gotMessage, &gotAck,
		&gotAckID, &binding, &agent, &last, &parent, &state); err != nil {
		t.Fatal(err)
	}
	if origin != (store.Origin{Source: "legacy", Subject: "KubePodCrashLooping"}) || gotContext != "ctx" || gotTask != "task" ||
		gotTurn != "turn" || gotUser != "U1" || gotTurnTask != "task" || gotMessage != "1.2" || gotAck != "eyes" || gotAckID != "ack" ||
		binding != "ns/b" || agent != "kagent/sre" || last != 123456789 || parent != "parent" || state != "state" {
		t.Fatalf("migrated session origin=%+v context=%q task=%q turn=%q user=%q turnTask=%q message=%q ack=%q ackID=%q binding=%q agent=%q last=%d parent=%q state=%q; want every field preserved", origin, gotContext, gotTask, gotTurn, gotUser, gotTurnTask, gotMessage, gotAck, gotAckID, binding, agent, last, parent, state)
	}
	var alert string
	if err := s.a.read.QueryRowContext(ctx, `SELECT alert_name FROM feedback WHERE thread_ts = '1.1'`).Scan(&alert); err != nil || alert != "KubePodCrashLooping" {
		t.Fatalf("feedback alert_name = %q, %v; want the specialized table unchanged", alert, err)
	}
}

func containsAll(values []string, wanted ...string) bool {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range wanted {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}
