package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func validKey(k store.ConversationKey) bool {
	return k.Connection != "" && k.Channel != "" && k.Thread != ""
}

// UpsertSession implements store.Sessions.
func (s *Store) UpsertSession(ctx context.Context, sess store.Session) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.UpsertSession")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if sess.TraceParent == "" {
		sess.TraceParent, sess.TraceState = telemetry.Capture(ctx)
	}
	if !validKey(sess.Key) || sess.Binding == "" || sess.LastActivity.IsZero() {
		return fmt.Errorf("upsert session %+v: key, Binding and LastActivity are required: %w", sess.Key, store.ErrInvalid)
	}
	_, err := s.exec(ctx, `INSERT INTO sessions (connection, channel, thread_ts, context_id, task_id, binding, alert_name, agent_name, last_activity, trace_parent, trace_state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (connection, channel, thread_ts) DO UPDATE SET
			context_id = EXCLUDED.context_id, task_id = EXCLUDED.task_id,
			binding = EXCLUDED.binding, alert_name = EXCLUDED.alert_name, agent_name = EXCLUDED.agent_name, last_activity = EXCLUDED.last_activity,
			trace_parent = CASE WHEN EXCLUDED.trace_parent = '' THEN sessions.trace_parent ELSE EXCLUDED.trace_parent END,
			trace_state = CASE WHEN EXCLUDED.trace_parent = '' THEN sessions.trace_state ELSE EXCLUDED.trace_state END`,
		sess.Key.Connection, sess.Key.Channel, sess.Key.Thread, sess.ContextID, sess.TaskID, sess.Binding, sess.Alert, sess.Agent, sess.LastActivity, sess.TraceParent, sess.TraceState)
	if err != nil {
		return fmt.Errorf("upsert session %+v: %w", sess.Key, err)
	}
	return nil
}

// GetSession implements store.Sessions.
func (s *Store) GetSession(ctx context.Context, key store.ConversationKey, ttl time.Duration, now time.Time) (session store.Session, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.GetSession")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !validKey(key) || ttl <= 0 || now.IsZero() {
		return store.Session{}, fmt.Errorf("get session %+v: key, positive ttl and now are required: %w", key, store.ErrInvalid)
	}
	sess := store.Session{Key: key}
	var last time.Time
	err := s.pool.QueryRow(ctx, `SELECT context_id, task_id, turn_id, turn_user, turn_task, binding, alert_name, agent_name, last_activity, trace_parent, trace_state FROM sessions
		WHERE connection = $1 AND channel = $2 AND thread_ts = $3`, key.Connection, key.Channel, key.Thread).
		Scan(&sess.ContextID, &sess.TaskID, &sess.Turn.ID, &sess.Turn.User, &sess.TurnTask, &sess.Binding, &sess.Alert, &sess.Agent, &last, &sess.TraceParent, &sess.TraceState)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Session{}, fmt.Errorf("get session %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Session{}, fmt.Errorf("get session %+v: %w", key, err)
	}
	sess.LastActivity = utc(last)
	if last.Before(now.Add(-ttl)) {
		return sess, fmt.Errorf("get session %+v: %w", key, store.ErrExpired)
	}
	return sess, nil
}

// TouchSession implements store.Sessions.
func (s *Store) TouchSession(ctx context.Context, key store.ConversationKey, at time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.TouchSession")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !validKey(key) || at.IsZero() {
		return fmt.Errorf("touch session %+v: key and time are required: %w", key, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE sessions SET last_activity = $1 WHERE connection = $2 AND channel = $3 AND thread_ts = $4`,
		at, key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("touch session %+v", key))
}

// SetSessionTask implements store.SessionTasks.
func (s *Store) SetSessionTask(ctx context.Context, key store.ConversationKey, contextID, taskID string, turn store.TaskTurn) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.SetSessionTask")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !validKey(key) {
		return fmt.Errorf("set session task %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	parent, state := telemetry.Capture(ctx)
	turnID, turnUser, turnTask := turn.ID, turn.User, taskID
	if taskID == "" || turnID == "" { // no task, no turn; and a turn is recorded for the task it is given with
		turnID, turnUser, turnTask = "", "", ""
	}
	n, err := s.exec(ctx, `UPDATE sessions SET
			trace_parent = CASE WHEN $1::text = '' THEN trace_parent ELSE $1::text END,
			trace_state = CASE WHEN $1::text = '' THEN trace_state ELSE $2::text END,
			task_id = $3, turn_id = $4, turn_user = $5, turn_task = $6, context_id = CASE WHEN $7::text = '' THEN context_id ELSE $7::text END
		WHERE connection = $8 AND channel = $9 AND thread_ts = $10`,
		parent, state, taskID, turnID, turnUser, turnTask, contextID, key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("set session task %+v", key))
}

// ResetSession implements store.SessionTasks.
func (s *Store) ResetSession(ctx context.Context, key store.ConversationKey) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ResetSession")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !validKey(key) {
		return fmt.Errorf("reset session %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE sessions SET context_id = '', task_id = '', turn_id = '', turn_user = '', turn_task = ''
		WHERE connection = $1 AND channel = $2 AND thread_ts = $3`, key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("reset session %+v", key))
}

// ListActiveSessions implements store.Recovery.
func (s *Store) ListActiveSessions(ctx context.Context) (sessions []store.Session, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListActiveSessions")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	rows, err := s.pool.Query(ctx, `SELECT connection, channel, thread_ts, context_id, task_id, turn_id, turn_user, turn_task, binding, alert_name, agent_name, last_activity, trace_parent, trace_state
		FROM sessions WHERE task_id <> '' ORDER BY last_activity, connection, channel, thread_ts`)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	defer rows.Close()
	var out []store.Session
	for rows.Next() {
		var sess store.Session
		var last time.Time
		if err := rows.Scan(&sess.Key.Connection, &sess.Key.Channel, &sess.Key.Thread, &sess.ContextID, &sess.TaskID, &sess.Turn.ID, &sess.Turn.User, &sess.TurnTask, &sess.Binding, &sess.Alert, &sess.Agent, &last, &sess.TraceParent, &sess.TraceState); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		sess.LastActivity = utc(last)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	return out, nil
}

// ListQueuedSessions implements store.Recovery.
func (s *Store) ListQueuedSessions(ctx context.Context) (keys []store.ConversationKey, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListQueuedSessions")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	rows, err := s.pool.Query(ctx, `SELECT connection, channel, thread_ts FROM reply_queue
		WHERE delivered_at IS NULL GROUP BY connection, channel, thread_ts ORDER BY MIN(sequence)`)
	if err != nil {
		return nil, fmt.Errorf("list queued sessions: %w", err)
	}
	defer rows.Close()
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
