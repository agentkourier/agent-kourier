package store

import (
	"context"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Settlement is what SettleInterrupted did.
type Settlement struct {
	// Failed are the tasks it failed, as they now stand.
	Failed []*a2a.Task
	// Unreadable are the tasks whose stored record does not decode, which it failed by the store's columns alone (the
	// state and the times): the record keeps what it held, and the retention deletes the task as a settled one.
	Unreadable []a2a.TaskInfo
	// Skipped counts the tasks that changed since it read them, which it left as they are.
	Skipped int
}

// Interrupted counts the tasks it failed, readable or not.
func (s Settlement) Interrupted() int { return len(s.Failed) + len(s.Unreadable) }

// SettleInterrupted fails each task a restart or a takeover left submitted or working, with an agent message of text.
// A task paused on a question or an approval (input-required) survives, and its answer resumes it.
//
// It finds the tasks in a write, and fails each in a write of its own, so that each step runs the fence: a store that
// has lost the lease fails it even with nothing to settle. A task whose stored record does not decode is failed by its
// columns alone, and logged at WARN, so that it does not stay running for good; a task that changed since it was read
// is logged at WARN and left as it is. A read or a write that fails stops it with an error, and the settlement so far:
// the store, not one task, is at fault, and the takeover that called it must not go on.
//
// It fails every such task in the store, whichever process ran it, so call it only when no turn is running anywhere
// on the database: before serving, and on Postgres only where no other replica serves turns from it.
func (s *Store) SettleInterrupted(ctx context.Context, text string) (Settlement, error) {
	type interrupted struct {
		id, contextID string
		version       int64
		raw           []byte
	}
	var found []interrupted
	err := s.Write(ctx, func(q Querier) error {
		return q.Query(ctx, `SELECT id, context_id, version, payload FROM a2a_tasks WHERE state IN (`+marks(len(running))+`) ORDER BY id`, running,
			func(scan func(...any) error) error {
				var it interrupted
				if err := scan(&it.id, &it.contextID, &it.version, &it.raw); err != nil {
					return err
				}
				found = append(found, it)
				return nil
			})
	})
	var done Settlement
	if err != nil {
		return done, fmt.Errorf("fail the interrupted tasks: %w", err)
	}
	for _, it := range found {
		now := s.now()
		var raw []byte
		task, err := decode(it.raw)
		if err == nil {
			task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed, Timestamp: &now,
				Message: a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart(text))}
			raw, err = encode(task)
		}
		if err != nil {
			s.log.WarnContext(ctx, "fail an unreadable interrupted task by its state alone", "task", it.id, "error", err)
			task, raw = nil, it.raw
		}
		var n int64
		err = s.Write(ctx, func(q Querier) error {
			var err error
			n, err = q.Exec(ctx, `UPDATE a2a_tasks SET state = ?, status_at = ?, updated_at = ?, payload = ?, version = version + 1
				WHERE id = ? AND version = ?`, string(a2a.TaskStateFailed), nanos(now), nanos(now), raw, it.id, it.version)
			return err
		})
		if err != nil {
			return done, fmt.Errorf("fail the interrupted tasks: task %s: %w", it.id, err)
		}
		switch {
		case n == 0:
			s.log.WarnContext(ctx, "fail an interrupted task", "task", it.id,
				"error", fmt.Errorf("the task changed since it was read: %w", taskstore.ErrConcurrentModification))
			done.Skipped++
		case task == nil:
			done.Unreadable = append(done.Unreadable, a2a.TaskInfo{TaskID: a2a.TaskID(it.id), ContextID: it.contextID})
		default:
			done.Failed = append(done.Failed, task)
		}
	}
	return done, nil
}
