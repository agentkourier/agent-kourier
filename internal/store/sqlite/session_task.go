package sqlite

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// SetSessionTask implements store.SessionTasks.
func (s *Store) SetSessionTask(ctx context.Context, key store.ConversationKey, contextID, taskID string, turn store.TaskTurn) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.SetSessionTask")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(key) {
		return fmt.Errorf("set session task %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	parent, state := telemetry.Capture(ctx)
	turnID, turnUser, turnTask := turn.ID, turn.User, taskID
	if taskID == "" || turnID == "" { // no task, no turn; and a turn is recorded for the task it is given with
		turnID, turnUser, turnTask = "", "", ""
	}
	n, err := s.exec(ctx, `UPDATE sessions SET trace_parent = CASE WHEN ? = '' THEN trace_parent ELSE ? END, trace_state = CASE WHEN ? = '' THEN trace_state ELSE ? END, task_id = ?, turn_id = ?, turn_user = ?, turn_task = ?, context_id = CASE WHEN ? = '' THEN context_id ELSE ? END
		WHERE connection = ? AND channel = ? AND thread_ts = ?`,
		parent, parent, parent, state, taskID, turnID, turnUser, turnTask, contextID, contextID, key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("set session task %+v", key))
}

// ResetSession implements store.SessionTasks.
func (s *Store) ResetSession(ctx context.Context, key store.ConversationKey) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ResetSession")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(key) {
		return fmt.Errorf("reset session %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE sessions SET context_id = '', task_id = '', turn_id = '', turn_user = '', turn_task = ''
		WHERE connection = ? AND channel = ? AND thread_ts = ?`, key.Connection, key.Channel, key.Thread)
	return errIfUnchanged(n, err, fmt.Sprintf("reset session %+v", key))
}
