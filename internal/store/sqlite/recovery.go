package sqlite

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// ListActiveSessions implements store.Recovery.
func (s *adapter) ListActiveSessions(ctx context.Context) ([]store.Session, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT connection, channel, thread_ts, context_id, task_id, turn_id, turn_user, turn_task, turn_message_ts, turn_acknowledgment, turn_acknowledgment_id, binding, alert_name, agent_name, last_activity, trace_parent, trace_state
		FROM sessions WHERE task_id != '' ORDER BY last_activity, connection, channel, thread_ts`)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.Session
	for rows.Next() {
		var sess store.Session
		var last int64
		if err := rows.Scan(&sess.Key.Connection, &sess.Key.Channel, &sess.Key.Thread, &sess.ContextID, &sess.TaskID, &sess.Turn.ID, &sess.Turn.User, &sess.TurnTask, &sess.Turn.MessageRef, &sess.Turn.Acknowledgment, &sess.Turn.AcknowledgmentID, &sess.Binding, &sess.Alert, &sess.Agent, &last, &sess.TraceParent, &sess.TraceState); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		sess.LastActivity = fromTS(last)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	return out, nil
}

// ListQueuedSessions implements store.Recovery.
func (s *adapter) ListQueuedSessions(ctx context.Context) ([]store.ConversationKey, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT connection, channel, thread_ts FROM reply_queue
		WHERE delivered_at IS NULL GROUP BY connection, channel, thread_ts ORDER BY MIN(sequence)`)
	if err != nil {
		return nil, fmt.Errorf("list queued sessions: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.ConversationKey
	for rows.Next() {
		var k store.ConversationKey
		if err := rows.Scan(&k.Connection, &k.Channel, &k.Thread); err != nil {
			return nil, fmt.Errorf("scan queued key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queued sessions: %w", err)
	}
	return out, nil
}
