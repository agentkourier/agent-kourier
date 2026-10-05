package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func validKey(k store.ConversationKey) bool {
	return k.Connection != "" && k.Channel != "" && k.Thread != ""
}

// UpsertSession implements store.Sessions.
func (s *Store) UpsertSession(ctx context.Context, sess store.Session) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.UpsertSession")
	defer func() { finishStore(op, telemetryErr) }()
	if sess.TraceParent == "" {
		sess.TraceParent, sess.TraceState = telemetry.Capture(ctx)
	}
	if !validKey(sess.Key) || sess.Binding == "" || sess.LastActivity.IsZero() {
		return fmt.Errorf("upsert session %+v: key, Binding and LastActivity are required: %w", sess.Key, store.ErrInvalid)
	}
	_, err := s.exec(ctx, `INSERT INTO sessions (connection, channel, thread_ts, context_id, task_id, binding, alert_name, agent_name, last_activity, trace_parent, trace_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (connection, channel, thread_ts) DO UPDATE SET
			context_id = excluded.context_id, task_id = excluded.task_id,
			binding = excluded.binding, alert_name = excluded.alert_name, agent_name = excluded.agent_name, last_activity = excluded.last_activity,
			trace_parent = CASE WHEN excluded.trace_parent = '' THEN trace_parent ELSE excluded.trace_parent END,
			trace_state = CASE WHEN excluded.trace_parent = '' THEN trace_state ELSE excluded.trace_state END`,
		sess.Key.Connection, sess.Key.Channel, sess.Key.Thread, sess.ContextID, sess.TaskID, sess.Binding, sess.Alert, sess.Agent, ts(sess.LastActivity), sess.TraceParent, sess.TraceState)
	if err != nil {
		return fmt.Errorf("upsert session %+v: %w", sess.Key, err)
	}
	return nil
}

// GetSession implements store.Sessions.
func (s *Store) GetSession(ctx context.Context, key store.ConversationKey, ttl time.Duration, now time.Time) (telemetryResult0 store.Session, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.GetSession")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(key) || ttl <= 0 || now.IsZero() {
		return store.Session{}, fmt.Errorf("get session %+v: key, positive ttl and now are required: %w", key, store.ErrInvalid)
	}
	sess := store.Session{Key: key}
	var last int64
	err := s.read.QueryRowContext(ctx, `SELECT context_id, task_id, turn_id, turn_user, turn_task, binding, alert_name, agent_name, last_activity, trace_parent, trace_state FROM sessions
		WHERE connection = ? AND channel = ? AND thread_ts = ?`, key.Connection, key.Channel, key.Thread).
		Scan(&sess.ContextID, &sess.TaskID, &sess.Turn.ID, &sess.Turn.User, &sess.TurnTask, &sess.Binding, &sess.Alert, &sess.Agent, &last, &sess.TraceParent, &sess.TraceState)
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
func (s *Store) TouchSession(ctx context.Context, key store.ConversationKey, at time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.TouchSession")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(key) || at.IsZero() {
		return fmt.Errorf("touch session %+v: key and time are required: %w", key, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE sessions SET last_activity = ? WHERE connection = ? AND channel = ? AND thread_ts = ?`,
		ts(at), key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("touch session %+v", key))
}
