package sqlite

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// SetSessionTask implements store.SessionTasks.
func (s *Store) SetSessionTask(ctx context.Context, key store.ConversationKey, contextID, taskID string) error {
	if !validKey(key) {
		return fmt.Errorf("set session task %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE sessions SET task_id = ?, context_id = CASE WHEN ? = '' THEN context_id ELSE ? END
		WHERE connection = ? AND channel = ? AND thread_ts = ?`,
		taskID, contextID, contextID, key.Connection, key.Channel, key.ThreadTS)
	return errIfUnchanged(n, err, fmt.Sprintf("set session task %+v", key))
}
