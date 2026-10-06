package postgres

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

var _ store.StaleTasks = (*Store)(nil)

// ListStaleTasks implements store.StaleTasks. It is a read, and so is not fenced.
func (s *Store) ListStaleTasks(ctx context.Context, h store.SessionHorizons, limit int) (stale []store.StaleTask, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListStaleTasks")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !h.Valid() || limit < 1 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("list stale tasks: every horizon and a limit of 1 to %d are required: %w", math.MaxInt32, store.ErrInvalid)
	}
	past, args := pastHorizon(h)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `SELECT s.connection, s.channel, s.thread_ts, s.binding, s.context_id, s.task_id FROM sessions s
		WHERE `+past+` AND s.task_id <> ''
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("list stale tasks: %w", err)
	}
	defer rows.Close()
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

// EndStaleTask implements store.StaleTasks. It is a write like any other: the epoch is checked first, so only the leader
// ends a task. The session row is locked by the update that clears it, and the interactions are cancelled in the same
// transaction, so a session that moved on is left whole.
func (s *Store) EndStaleTask(ctx context.Context, h store.SessionHorizons, t store.StaleTask, now time.Time) (ended store.EndedStaleTask, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.EndStaleTask")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !h.Valid() || !validKey(t.Key) || t.TaskID == "" || now.IsZero() {
		return store.EndedStaleTask{}, fmt.Errorf("end stale task %+v: every horizon, a complete key, a task and a time are required: %w", t.Key, store.ErrInvalid)
	}
	past, args := pastHorizon(h)
	n := len(args)
	args = append(args, t.Key.Connection, t.Key.Channel, t.Key.Thread, t.TaskID)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE sessions s SET task_id = '', turn_id = '', turn_user = '', turn_task = ''
			WHERE s.connection = $%d AND s.channel = $%d AND s.thread_ts = $%d AND s.task_id = $%d AND %s`, n+1, n+2, n+3, n+4, past), args...)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		ended.Ended = true
		tag, err = tx.Exec(ctx, `UPDATE interactions SET state = 'cancelled', decided_at = $1
			WHERE connection = $2 AND channel = $3 AND thread_ts = $4 AND task_id = $5 AND state = 'pending'`,
			now, t.Key.Connection, t.Key.Channel, t.Key.Thread, t.TaskID)
		ended.Interactions = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return store.EndedStaleTask{}, fmt.Errorf("end stale task %+v: %w", t.Key, err)
	}
	return ended, nil
}
