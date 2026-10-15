package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// UpsertSession implements store.Sessions.
func (s *adapter) UpsertSession(ctx context.Context, sess store.Session) error {
	_, err := s.exec(ctx, `INSERT INTO sessions (connection, channel, thread_ts, context_id, task_id, binding, origin_source, origin_subject, agent_name, last_activity, trace_parent, trace_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (connection, channel, thread_ts) DO UPDATE SET
			context_id = excluded.context_id, task_id = excluded.task_id,
			binding = excluded.binding, origin_source = excluded.origin_source, origin_subject = excluded.origin_subject,
			agent_name = excluded.agent_name, last_activity = excluded.last_activity,
			trace_parent = CASE WHEN excluded.trace_parent = '' THEN trace_parent ELSE excluded.trace_parent END,
			trace_state = CASE WHEN excluded.trace_parent = '' THEN trace_state ELSE excluded.trace_state END`,
		sess.Key.Connection, sess.Key.Channel, sess.Key.Thread, sess.ContextID, sess.TaskID, sess.Binding, sess.Origin.Source, sess.Origin.Subject, sess.Agent, ts(sess.LastActivity), sess.TraceParent, sess.TraceState)
	if err != nil {
		return fmt.Errorf("upsert session %+v: %w", sess.Key, err)
	}
	return nil
}

// GetSession implements store.Sessions.
func (s *adapter) GetSession(ctx context.Context, key store.ConversationKey, ttl time.Duration, now time.Time) (store.Session, error) {
	sess := store.Session{Key: key}
	var last int64
	err := s.read.QueryRowContext(ctx, `SELECT context_id, task_id, turn_id, turn_user, turn_task, turn_message_ts, turn_acknowledgment, turn_acknowledgment_id, binding, origin_source, origin_subject, agent_name, last_activity, trace_parent, trace_state FROM sessions
		WHERE connection = ? AND channel = ? AND thread_ts = ?`, key.Connection, key.Channel, key.Thread).
		Scan(&sess.ContextID, &sess.TaskID, &sess.Turn.ID, &sess.Turn.User, &sess.TurnTask, &sess.Turn.MessageRef, &sess.Turn.Acknowledgment, &sess.Turn.AcknowledgmentID, &sess.Binding, &sess.Origin.Source, &sess.Origin.Subject, &sess.Agent, &last, &sess.TraceParent, &sess.TraceState)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, fmt.Errorf("get session %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Session{}, fmt.Errorf("get session %+v: %w", key, err)
	}
	sess.LastActivity = fromTS(last)
	if last < ts(now.Add(-ttl)) {
		return sess, fmt.Errorf("get session %+v: %w", key, store.ErrExpired)
	}
	return sess, nil
}

// TouchSession implements store.Sessions.
func (s *adapter) TouchSession(ctx context.Context, key store.ConversationKey, at time.Time) error {
	n, err := s.exec(ctx, `UPDATE sessions SET last_activity = ? WHERE connection = ? AND channel = ? AND thread_ts = ?`,
		ts(at), key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("touch session %+v", key))
}
