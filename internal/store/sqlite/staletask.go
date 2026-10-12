package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.StaleTasks = (*adapter)(nil)

// endStaleTaskQuery is the statement that clears the task t of a session if the session is still past its horizon, and
// its arguments.
func endStaleTaskQuery(h store.SessionHorizons, t store.StaleTask) (string, []any) {
	b := store.NewBinder(Dialect{})
	key := `s.connection = ` + b.Arg(t.Key.Connection) + ` AND s.channel = ` + b.Arg(t.Key.Channel) + ` AND s.thread_ts = ` + b.Arg(t.Key.Thread)
	task := `s.task_id = ` + b.Arg(t.TaskID)
	return `UPDATE sessions AS s SET task_id = '', turn_id = '', turn_user = '', turn_task = '', turn_message_ts = '', turn_acknowledgment = '', turn_acknowledgment_id = ''
			WHERE ` + key + ` AND ` + task + ` AND ` + b.Render(store.PastHorizon("s", h)), b.Args()
}

// ListStaleTasks implements store.StaleTasks.
func (s *adapter) ListStaleTasks(ctx context.Context, h store.SessionHorizons, limit int) ([]store.StaleTask, error) {
	b := store.NewBinder(Dialect{})
	past := b.Render(store.PastHorizon("s", h))
	lim := b.Arg(limit)
	var stale []store.StaleTask
	//nolint:gosec // G202: past is column names and placeholders; every value is an argument
	rows, err := s.read.QueryContext(ctx, `SELECT s.connection, s.channel, s.thread_ts, s.binding, s.context_id, s.task_id FROM sessions s
		WHERE `+past+` AND s.task_id <> ''
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT `+lim, b.Args()...)
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

// StaleTaskTurn implements store.StaleTaskTurnReader.
func (s *adapter) StaleTaskTurn(ctx context.Context, key store.ConversationKey, taskID string) (store.TaskTurn, error) {
	var turn store.TaskTurn
	err := s.read.QueryRowContext(ctx, `SELECT turn_id, turn_user, turn_message_ts, turn_acknowledgment, turn_acknowledgment_id FROM sessions
		WHERE connection = ? AND channel = ? AND thread_ts = ? AND task_id = ?`,
		key.Connection, key.Channel, key.Thread, taskID).Scan(&turn.ID, &turn.User, &turn.MessageRef, &turn.Acknowledgment, &turn.AcknowledgmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.TaskTurn{}, fmt.Errorf("stale task turn %+v/%s: %w", key, taskID, store.ErrNotFound)
	}
	if err != nil {
		return store.TaskTurn{}, fmt.Errorf("stale task turn %+v/%s: %w", key, taskID, err)
	}
	return turn, nil
}

// EndStaleTask implements store.StaleTasks. The session is cleared only while it is still past its horizon and still records
// the task, and the interactions are cancelled in the same transaction, so a session that moved on is left whole.
func (s *adapter) EndStaleTask(ctx context.Context, h store.SessionHorizons, t store.StaleTask, now time.Time) (store.EndedStaleTask, error) {
	endTask, args := endStaleTaskQuery(h, t)
	var ended store.EndedStaleTask
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, endTask, args...)
		if err != nil || n == 0 {
			return err
		}
		ended.Ended = true
		n, err = execRows(ctx, tx, `UPDATE interactions SET state = 'cancelled', decided_at = ?
			WHERE connection = ? AND channel = ? AND thread_ts = ? AND task_id = ? AND `+store.PendingInteractions("").Text(Dialect{}),
			ts(now), t.Key.Connection, t.Key.Channel, t.Key.Thread, t.TaskID)
		ended.Interactions = int(n)
		return err
	})
	if err != nil {
		return store.EndedStaleTask{}, fmt.Errorf("end stale task %+v: %w", t.Key, err)
	}
	return ended, nil
}
