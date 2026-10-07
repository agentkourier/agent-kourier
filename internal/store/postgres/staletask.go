package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.StaleTasks = (*adapter)(nil)

// endStaleTaskQuery is the statement that clears the task t of a session if the session is still past its horizon, and
// its arguments.
func endStaleTaskQuery(h store.SessionHorizons, t store.StaleTask) (string, []any) {
	b := store.NewBinder(Dialect{})
	key := `s.connection = ` + b.Arg(t.Key.Connection) + ` AND s.channel = ` + b.Arg(t.Key.Channel) + ` AND s.thread_ts = ` + b.Arg(t.Key.Thread)
	task := `s.task_id = ` + b.Arg(t.TaskID)
	return `UPDATE sessions s SET task_id = '', turn_id = '', turn_user = '', turn_task = ''
			WHERE ` + key + ` AND ` + task + ` AND ` + b.Render(store.PastHorizon("s", h)), b.Args()
}

// ListStaleTasks implements store.StaleTasks. It is a read, and so is not fenced.
func (s *adapter) ListStaleTasks(ctx context.Context, h store.SessionHorizons, limit int) ([]store.StaleTask, error) {
	var stale []store.StaleTask
	b := store.NewBinder(Dialect{})
	past := b.Render(store.PastHorizon("s", h))
	lim := b.Arg(limit)
	rows, err := s.pool.Query(ctx, `SELECT s.connection, s.channel, s.thread_ts, s.binding, s.context_id, s.task_id FROM sessions s
		WHERE `+past+` AND s.task_id <> ''
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT `+lim, b.Args()...)
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
func (s *adapter) EndStaleTask(ctx context.Context, h store.SessionHorizons, t store.StaleTask, now time.Time) (store.EndedStaleTask, error) {
	var ended store.EndedStaleTask
	endTask, args := endStaleTaskQuery(h, t)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, endTask, args...)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		ended.Ended = true
		tag, err = tx.Exec(ctx, `UPDATE interactions SET state = 'cancelled', decided_at = $1
			WHERE connection = $2 AND channel = $3 AND thread_ts = $4 AND task_id = $5 AND `+store.PendingInteractions("").Text(Dialect{}),
			now, t.Key.Connection, t.Key.Channel, t.Key.Thread, t.TaskID)
		ended.Interactions = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return store.EndedStaleTask{}, fmt.Errorf("end stale task %+v: %w", t.Key, err)
	}
	return ended, nil
}
