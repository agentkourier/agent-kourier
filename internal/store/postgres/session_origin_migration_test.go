package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestMigration0026MovesLegacySessionAlertIntoOrigin(t *testing.T) {
	dsn, schema, _ := freshSchema(t)
	ctx := t.Context()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	migrateUpTo(t, p, 25)
	lastAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err = p.Exec(ctx, `INSERT INTO sessions
		(connection, channel, thread_ts, context_id, task_id, turn_id, turn_user, turn_task, turn_message_ts,
		 turn_acknowledgment, turn_acknowledgment_id, binding, alert_name, agent_name, last_activity, trace_parent, trace_state)
		VALUES ('slack', 'C1', '1.1', 'ctx', 'task', 'turn', 'U1', 'task', '1.2', 'eyes', 'ack', 'ns/b',
		 'KubePodCrashLooping', 'kagent/sre', $1, 'parent', 'state')`, lastAt)
	if err != nil {
		p.Close()
		t.Fatalf("insert legacy session: %v", err)
	}
	_, err = p.Exec(ctx, `INSERT INTO feedback
		(connection, channel, thread_ts, turn_id, user_id, binding, agent, alert_name, kind, verdict, first_at, voted_at)
		VALUES ('slack', 'C1', '1.1', 'turn', 'U1', 'ns/b', 'kagent/sre', 'KubePodCrashLooping', 'investigation', 'useful', $1, $1)`, lastAt)
	if err != nil {
		p.Close()
		t.Fatalf("insert feedback row: %v", err)
	}
	p.Close()

	s := openAt(t, dsn)
	rows, err := s.a.pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'sessions' ORDER BY ordinal_position`, schema)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if containsString(columns, "alert_name") || !containsString(columns, "origin_source") || !containsString(columns, "origin_subject") {
		t.Fatalf("sessions columns = %v; want origin columns and no alert_name", columns)
	}

	var origin store.Origin
	var gotContext, gotTask, gotTurn, gotUser, gotTurnTask, gotMessage, gotAck, gotAckID, binding, agent, parent, state string
	var gotLast time.Time
	if err := s.a.pool.QueryRow(ctx, `SELECT origin_source, origin_subject, context_id, task_id, turn_id, turn_user,
		turn_task, turn_message_ts, turn_acknowledgment, turn_acknowledgment_id, binding, agent_name, last_activity,
		trace_parent, trace_state FROM sessions WHERE connection = 'slack' AND channel = 'C1' AND thread_ts = '1.1'`).Scan(
		&origin.Source, &origin.Subject, &gotContext, &gotTask, &gotTurn, &gotUser, &gotTurnTask, &gotMessage, &gotAck,
		&gotAckID, &binding, &agent, &gotLast, &parent, &state); err != nil {
		t.Fatal(err)
	}
	if origin != (store.Origin{Source: "legacy", Subject: "KubePodCrashLooping"}) || gotContext != "ctx" || gotTask != "task" ||
		gotTurn != "turn" || gotUser != "U1" || gotTurnTask != "task" || gotMessage != "1.2" || gotAck != "eyes" || gotAckID != "ack" ||
		binding != "ns/b" || agent != "kagent/sre" || !gotLast.Equal(lastAt) || parent != "parent" || state != "state" {
		t.Fatalf("migrated session origin=%+v context=%q task=%q turn=%q user=%q turnTask=%q message=%q ack=%q ackID=%q binding=%q agent=%q last=%v parent=%q state=%q; want every field preserved", origin, gotContext, gotTask, gotTurn, gotUser, gotTurnTask, gotMessage, gotAck, gotAckID, binding, agent, gotLast, parent, state)
	}
	var alert string
	if err := s.a.pool.QueryRow(ctx, `SELECT alert_name FROM feedback WHERE thread_ts = '1.1'`).Scan(&alert); err != nil || alert != "KubePodCrashLooping" {
		t.Fatalf("feedback alert_name = %q, %v; want the specialized table unchanged", alert, err)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
