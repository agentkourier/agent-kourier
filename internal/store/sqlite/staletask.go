package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

var _ store.StaleTasks = (*Store)(nil)

// ListStaleTasks implements store.StaleTasks.
func (s *Store) ListStaleTasks(ctx context.Context, h store.SessionHorizons, limit int) (stale []store.StaleTask, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListStaleTasks")
	defer func() { finishStore(op, telemetryErr) }()
	if !h.Valid() || limit < 1 {
		return nil, fmt.Errorf("list stale tasks: every horizon and a positive limit are required: %w", store.ErrInvalid)
	}
	past, args := pastHorizon(h)
	//nolint:gosec // G202: past is column names and placeholders; every value is an argument
	rows, err := s.read.QueryContext(ctx, `SELECT s.connection, s.channel, s.thread_ts, s.binding, s.context_id, s.task_id FROM sessions s
		WHERE `+past+` AND s.task_id <> ''
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list stale tasks: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	for rows.Next() {
		var t store.StaleTask
		if err := rows.Scan(&t.Key.Connection, &t.Key.Channel, &t.Key.Thread, &t.Binding, &t.ContextID, &t.TaskID); err != nil {
			return nil, fmt.Errorf("list stale tasks: scan: %w", err)
		}
		stale = append(stale, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale tasks: %w", err)
	}
	return stale, nil
}

// EndStaleTask implements store.StaleTasks. The session is cleared only while it is still past its horizon and still records
// the task, and the interactions are cancelled in the same transaction, so a session that moved on is left whole.
func (s *Store) EndStaleTask(ctx context.Context, h store.SessionHorizons, t store.StaleTask, now time.Time) (ended store.EndedStaleTask, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.EndStaleTask")
	defer func() { finishStore(op, telemetryErr) }()
	if !h.Valid() || !validKey(t.Key) || t.TaskID == "" || now.IsZero() {
		return store.EndedStaleTask{}, fmt.Errorf("end stale task %+v: every horizon, a complete key, a task and a time are required: %w", t.Key, store.ErrInvalid)
	}
	past, args := pastHorizon(h)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE sessions AS s SET task_id = '', turn_id = '', turn_user = '', turn_task = ''
			WHERE s.connection = ? AND s.channel = ? AND s.thread_ts = ? AND s.task_id = ? AND `+past,
			append([]any{t.Key.Connection, t.Key.Channel, t.Key.Thread, t.TaskID}, args...)...)
		if err != nil || n == 0 {
			return err
		}
		ended.Ended = true
		n, err = execRows(ctx, tx, `UPDATE interactions SET state = 'cancelled', decided_at = ?
			WHERE connection = ? AND channel = ? AND thread_ts = ? AND task_id = ? AND state = 'pending'`,
			ts(now), t.Key.Connection, t.Key.Channel, t.Key.Thread, t.TaskID)
		ended.Interactions = int(n)
		return err
	})
	if err != nil {
		return store.EndedStaleTask{}, fmt.Errorf("end stale task %+v: %w", t.Key, err)
	}
	return ended, nil
}
