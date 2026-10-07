package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const triggerThreadColumns = `binding, key_hash, thread, opened, last_seen, closed_at, cooldown_until`

// scanTriggerThread reads one row selected with triggerThreadColumns.
func scanTriggerThread(r pgx.Row) (store.TriggerThread, error) {
	var t store.TriggerThread
	var opened, lastSeen time.Time
	var closed, cooldown *time.Time
	if err := r.Scan(&t.Binding, &t.KeyHash, &t.Thread, &opened, &lastSeen, &closed, &cooldown); err != nil {
		return store.TriggerThread{}, err
	}
	t.Opened, t.LastSeen = utc(opened), utc(lastSeen)
	t.ClosedAt, t.CooldownUntil = fromNull(closed), fromNull(cooldown)
	return t, nil
}

// openTriggerThreadQuery is the upsert that reserves the key of t, and its arguments: the row is replaced only where
// store.TriggerThreadReplaceable says so.
func openTriggerThreadQuery(t store.TriggerThread, stale store.TriggerStale) (string, []any) {
	b := store.NewBinder(Dialect{})
	binding, keyHash, thread, opened := b.Arg(t.Binding), b.Arg(t.KeyHash), b.Arg(t.Thread), b.Arg(t.Opened)
	replaceable := b.Render(store.TriggerThreadReplaceable(stale))
	return `INSERT INTO trigger_threads (` + triggerThreadColumns + `) VALUES (` + binding + `, ` + keyHash + `, ` + thread + `, ` + opened + `, ` + opened + `, NULL, NULL)
			ON CONFLICT (binding, key_hash) DO UPDATE SET thread = EXCLUDED.thread, opened = EXCLUDED.opened,
				last_seen = EXCLUDED.last_seen, closed_at = NULL, cooldown_until = NULL
			WHERE ` + replaceable + `
			RETURNING ` + triggerThreadColumns, b.Args()
}

// OpenTriggerThread implements store.TriggerThreads. The upsert's WHERE is the rule for replacing a row, and
// Postgres makes the upsert atomic by row: of two starts of a key at once the second waits for the first, and then
// weighs the row the first left, so both cannot win. A new row has no cooldown, which is a turn that has not started
// (MarkTriggerStarted). A cooldown or a close that is NULL is "never set": a closed thread with no cooldown is past it.
func (s *adapter) OpenTriggerThread(ctx context.Context, t store.TriggerThread, stale store.TriggerStale) (store.TriggerThread, bool, error) {
	var current store.TriggerThread
	var opened bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		query, args := openTriggerThreadQuery(t, stale)
		row, err := scanTriggerThread(tx.QueryRow(ctx, query, args...))
		if err == nil {
			current, opened = row, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The row stood, and is the answer: the row as it is now, which a concurrent start may have just written.
		current, err = scanTriggerThread(tx.QueryRow(ctx, `SELECT `+triggerThreadColumns+` FROM trigger_threads WHERE binding = $1 AND key_hash = $2`, t.Binding, t.KeyHash))
		return err
	})
	if err != nil {
		return store.TriggerThread{}, false, fmt.Errorf("open trigger thread of %q: %w", t.Binding, err)
	}
	return current, opened, nil
}

// MarkTriggerStarted implements store.TriggerThreads.
func (s *adapter) MarkTriggerStarted(ctx context.Context, binding, keyHash, thread string, cooldownUntil time.Time) error {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE trigger_threads SET cooldown_until = $1 WHERE binding = $2 AND key_hash = $3 AND thread = $4`, cooldownUntil, binding, keyHash, thread)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		var exists bool // no change: the row is missing, or another message's
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM trigger_threads WHERE binding = $1 AND key_hash = $2)`, binding, keyHash).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return store.ErrNotFound
		}
		return fmt.Errorf("the key's thread is not %s: %w", thread, store.ErrConflict)
	})
	if err != nil {
		return fmt.Errorf("mark trigger thread of %q started: %w", binding, err)
	}
	return nil
}

// ReleaseTriggerThread implements store.TriggerThreads.
func (s *adapter) ReleaseTriggerThread(ctx context.Context, binding, keyHash, thread string) (bool, error) {
	n, err := s.exec(ctx, `DELETE FROM trigger_threads WHERE binding = $1 AND key_hash = $2 AND thread = $3 AND `+store.TriggerThreadUnstarted("").Text(Dialect{}), binding, keyHash, thread)
	if err != nil {
		return false, fmt.Errorf("release trigger thread of %q: %w", binding, err)
	}
	return n > 0, nil
}

// GetTriggerThread implements store.TriggerThreads.
func (s *adapter) GetTriggerThread(ctx context.Context, binding, keyHash string) (store.TriggerThread, error) {
	t, err := scanTriggerThread(s.pool.QueryRow(ctx, `SELECT `+triggerThreadColumns+` FROM trigger_threads WHERE binding = $1 AND key_hash = $2`, binding, keyHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TriggerThread{}, fmt.Errorf("get trigger thread of %q: %w", binding, store.ErrNotFound)
	}
	if err != nil {
		return store.TriggerThread{}, fmt.Errorf("get trigger thread of %q: %w", binding, err)
	}
	return t, nil
}

// ReopenTriggerThread implements store.TriggerThreads.
func (s *adapter) ReopenTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) error {
	n, err := s.exec(ctx, `UPDATE trigger_threads SET last_seen = GREATEST(last_seen, $1), closed_at = NULL WHERE binding = $2 AND key_hash = $3`, at, binding, keyHash)
	return errIfUnchanged(n, err, fmt.Sprintf("reopen trigger thread of %q", binding))
}

// CloseTriggerThread implements store.TriggerThreads.
func (s *adapter) CloseTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (bool, error) {
	var closed bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE trigger_threads SET closed_at = $1, last_seen = GREATEST(last_seen, $1) WHERE binding = $2 AND key_hash = $3 AND `+store.TriggerThreadOpen("").Text(Dialect{}), at, binding, keyHash)
		if err != nil || tag.RowsAffected() > 0 {
			closed = tag.RowsAffected() > 0
			return err
		}
		var exists bool // no change: the row is missing, or closed already
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM trigger_threads WHERE binding = $1 AND key_hash = $2)`, binding, keyHash).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return store.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("close trigger thread of %q: %w", binding, err)
	}
	return closed, nil
}

// PurgeTriggerThreads implements store.TriggerThreads.
func (s *adapter) PurgeTriggerThreads(ctx context.Context, now, idleBefore time.Time) (int, error) {
	b := store.NewBinder(Dialect{})
	n, err := s.exec(ctx, `DELETE FROM trigger_threads WHERE `+b.Render(store.TriggerThreadPurgeable(now, idleBefore)), b.Args()...)
	if err != nil {
		return 0, fmt.Errorf("purge trigger threads: %w", err)
	}
	return int(n), nil
}
