package store

import (
	"context"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// running are the states of a task whose turn is under way; settled, the terminal ones, in which a task never changes
// again.
var (
	running = []any{string(a2a.TaskStateSubmitted), string(a2a.TaskStateWorking)}
	settled = []any{string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed), string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected)}
)

// DeleteSettledBefore deletes the settled tasks last updated before cutoff, and their message IDs, and returns how
// many tasks it deleted.
func (s *Store) DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	filter := `state IN (` + marks(len(settled)) + `) AND updated_at < ?`
	args := append(append([]any{}, settled...), nanos(cutoff))
	err := s.Write(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM message_ids WHERE task_id IN (SELECT id FROM a2a_tasks WHERE `+filter+`)`, args...); err != nil {
			return err
		}
		var err error
		n, err = q.Exec(ctx, `DELETE FROM a2a_tasks WHERE `+filter, args...)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete settled tasks: %w", err)
	}
	return n, nil
}

// DeleteIdleContexts deletes each context last active before cutoff, with no turn running: what the add-on keeps of it
// (TaskOptions.ReleaseContext), its tasks (a question it paused on included) and their message IDs. The context's next
// message starts it afresh. It returns how many contexts it deleted. Each context goes in a transaction of its own,
// which checks again that the context is still idle.
func (s *Store) DeleteIdleContexts(ctx context.Context, cutoff time.Time) (int, error) {
	var found []Context
	err := s.Reader().Query(ctx, `SELECT owner, context_id, last_active, ref FROM contexts WHERE last_active < ? ORDER BY last_active`,
		[]any{nanos(cutoff)}, func(scan func(...any) error) error {
			var c Context
			var at int64
			if err := scan(&c.Owner, &c.ContextID, &at, &c.Ref); err != nil {
				return err
			}
			c.LastActive = fromNanos(at)
			found = append(found, c)
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("find idle contexts: %w", err)
	}
	deleted := 0
	for _, c := range found {
		gone := false
		err := s.Write(ctx, func(q Querier) error {
			n, err := q.Exec(ctx, `DELETE FROM contexts WHERE owner = ? AND context_id = ? AND last_active < ?
				AND NOT EXISTS (SELECT 1 FROM a2a_tasks WHERE owner = ? AND context_id = ? AND state IN (`+marks(len(running))+`))`,
				append([]any{c.Owner, c.ContextID, nanos(cutoff), c.Owner, c.ContextID}, running...)...)
			if err != nil || n == 0 {
				return err // n == 0: active again, or a turn is running
			}
			if s.releaseContext != nil {
				if err := s.releaseContext(ctx, q, c); err != nil {
					return err
				}
			}
			if _, err := q.Exec(ctx, `DELETE FROM message_ids WHERE owner = ? AND task_id IN (SELECT id FROM a2a_tasks WHERE owner = ? AND context_id = ?)`,
				c.Owner, c.Owner, c.ContextID); err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `DELETE FROM a2a_tasks WHERE owner = ? AND context_id = ?`, c.Owner, c.ContextID); err != nil {
				return err
			}
			gone = true
			return nil
		})
		if err != nil {
			return deleted, fmt.Errorf("delete idle context %s: %w", c.ContextID, err)
		}
		if gone {
			deleted++
		}
	}
	return deleted, nil
}
