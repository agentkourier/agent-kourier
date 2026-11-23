package store

import (
	"context"
	"fmt"
	"time"
)

// Context is an owner's A2A context as the store records it.
type Context struct {
	// Owner is the caller whose context it is.
	Owner string
	// ContextID is the A2A context's ID.
	ContextID string
	// LastActive is when the context was last active (TaskOptions.ContextActivity, TouchContext, PutContext,
	// TouchContextRef).
	LastActive time.Time
	// Ref is the add-on's own handle on the context, such as the ID of the backend session that serves it; empty for
	// an add-on that keeps none.
	Ref string
}

// touch records now as the owner's context's last activity, and keeps its ref.
func touch(ctx context.Context, q Querier, user, contextID string, now int64) error {
	_, err := q.Exec(ctx, `INSERT INTO contexts (owner, context_id, last_active) VALUES (?, ?, ?)
		ON CONFLICT (owner, context_id) DO UPDATE SET last_active = excluded.last_active`, user, contextID, now)
	return err
}

// TouchContext records now as the last activity of the call's caller's context, in a transaction of its own. An
// add-on calls it when a message for the context arrives, before the turn reads what it keeps of the context, so that
// DeleteIdleContexts, which deletes a context only if it is still idle in its own transaction, keeps it.
func (s *Store) TouchContext(ctx context.Context, contextID string) error {
	user, err := owner(ctx)
	if err != nil {
		return err
	}
	if err := s.Write(ctx, func(q Querier) error { return touch(ctx, q, user, contextID, nanos(s.now())) }); err != nil {
		return fmt.Errorf("touch context %s: %w", contextID, err)
	}
	return nil
}

// Contexts returns every owner's contexts, by owner and context ID.
func (s *Store) Contexts(ctx context.Context) ([]Context, error) {
	var out []Context
	err := s.Reader().Query(ctx, `SELECT owner, context_id, last_active, ref FROM contexts ORDER BY owner, context_id`, nil,
		func(scan func(...any) error) error {
			var c Context
			var at int64
			if err := scan(&c.Owner, &c.ContextID, &at, &c.Ref); err != nil {
				return err
			}
			c.LastActive = fromNanos(at)
			out = append(out, c)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list contexts: %w", err)
	}
	return out, nil
}

// PutContext records c's ref and last activity, replacing the ones its context had.
func (s *Store) PutContext(ctx context.Context, c Context) error {
	err := s.Write(ctx, func(q Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO contexts (owner, context_id, last_active, ref) VALUES (?, ?, ?, ?)
			ON CONFLICT (owner, context_id) DO UPDATE SET last_active = excluded.last_active, ref = excluded.ref`,
			c.Owner, c.ContextID, nanos(c.LastActive), c.Ref)
		return err
	})
	if err != nil {
		return fmt.Errorf("put context %s: %w", c.ContextID, err)
	}
	return nil
}

// TouchContextRef sets the last activity of the owner's context to at, while the context's ref is still ref: a touch
// for a ref the context no longer has changes nothing.
func (s *Store) TouchContextRef(ctx context.Context, owner, contextID, ref string, at time.Time) error {
	err := s.Write(ctx, func(q Querier) error {
		_, err := q.Exec(ctx, `UPDATE contexts SET last_active = ? WHERE owner = ? AND context_id = ? AND ref = ?`,
			nanos(at), owner, contextID, ref)
		return err
	})
	if err != nil {
		return fmt.Errorf("touch context %s: %w", contextID, err)
	}
	return nil
}

// ForgetContext is for an owner's context whose ref the add-on has let go of (an archived session). It removes the
// context, if its ref is still ref, and deletes the owner's tasks of the context last updated before cutoff, with their
// message IDs: they belong to what ref named and can never run again. A task the context started after cutoff, under
// a new ref, is kept.
func (s *Store) ForgetContext(ctx context.Context, owner, contextID, ref string, cutoff time.Time) error {
	err := s.Write(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM contexts WHERE owner = ? AND context_id = ? AND ref = ?`, owner, contextID, ref); err != nil {
			return err
		}
		filter := `owner = ? AND context_id = ? AND updated_at < ?`
		args := []any{owner, contextID, nanos(cutoff)}
		if _, err := q.Exec(ctx, `DELETE FROM message_ids WHERE owner = ? AND task_id IN (SELECT id FROM a2a_tasks WHERE `+filter+`)`,
			append([]any{owner}, args...)...); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `DELETE FROM a2a_tasks WHERE `+filter, args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("forget context %s: %w", contextID, err)
	}
	return nil
}
