package store

import (
	"context"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// SettleInterrupted fails each task a restart or a takeover left submitted or working, with an agent message of text,
// and returns the tasks it failed, as they now stand. A task paused on a question or an approval (input-required)
// survives, and its answer resumes it.
//
// It finds the tasks in a write, and fails each in a write of its own, so that each step runs the fence: a store that
// has lost the lease fails it even with nothing to settle. A task it cannot fail for a reason of its own (a stored
// record that does not decode, or a task that changed since it was read) is logged at WARN and left as it is, and the
// others are still failed. A read or a write that fails stops it with an error: the store, not one task, is at fault,
// and the takeover that called it must not go on.
//
// It fails every such task in the store, whichever process ran it, so call it only when no turn is running anywhere
// on the database: before serving, and on Postgres only where no other replica serves turns from it.
func (s *Store) SettleInterrupted(ctx context.Context, text string) ([]*a2a.Task, error) {
	type interrupted struct {
		id      string
		version int64
		raw     []byte
	}
	var found []interrupted
	err := s.Write(ctx, func(q Querier) error {
		return q.Query(ctx, `SELECT id, version, payload FROM a2a_tasks WHERE state IN (`+marks(len(running))+`) ORDER BY id`, running,
			func(scan func(...any) error) error {
				var it interrupted
				if err := scan(&it.id, &it.version, &it.raw); err != nil {
					return err
				}
				found = append(found, it)
				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("fail the interrupted tasks: %w", err)
	}
	var failed []*a2a.Task
	for _, it := range found {
		task, err := decode(it.raw)
		if err != nil {
			s.log.WarnContext(ctx, "fail an interrupted task", "task", it.id, "error", err)
			continue
		}
		now := s.now()
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed, Timestamp: &now,
			Message: a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart(text))}
		raw, err := encode(task)
		if err != nil {
			s.log.WarnContext(ctx, "fail an interrupted task", "task", it.id, "error", err)
			continue
		}
		var n int64
		err = s.Write(ctx, func(q Querier) error {
			var err error
			n, err = q.Exec(ctx, `UPDATE a2a_tasks SET state = ?, status_at = ?, updated_at = ?, payload = ?, version = version + 1
				WHERE id = ? AND version = ?`, string(a2a.TaskStateFailed), nanos(now), nanos(now), raw, it.id, it.version)
			return err
		})
		if err != nil {
			return failed, fmt.Errorf("fail the interrupted tasks: task %s: %w", it.id, err)
		}
		if n == 0 {
			s.log.WarnContext(ctx, "fail an interrupted task", "task", it.id,
				"error", fmt.Errorf("the task changed since it was read: %w", taskstore.ErrConcurrentModification))
			continue
		}
		failed = append(failed, task)
	}
	return failed, nil
}
