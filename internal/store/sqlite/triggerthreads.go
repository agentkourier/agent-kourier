package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const triggerThreadColumns = `binding, key_hash, thread, opened, last_seen, closed_at, cooldown_until`

// scanTriggerThread reads one row selected with triggerThreadColumns.
func scanTriggerThread(r interface{ Scan(...any) error }) (store.TriggerThread, error) {
	var t store.TriggerThread
	var opened, lastSeen, closed, cooldown int64
	if err := r.Scan(&t.Binding, &t.KeyHash, &t.Thread, &opened, &lastSeen, &closed, &cooldown); err != nil {
		return store.TriggerThread{}, err
	}
	t.Opened, t.LastSeen, t.CooldownUntil = fromTS(opened), fromTS(lastSeen), fromTS(cooldown)
	if closed != 0 {
		t.ClosedAt = fromTS(closed)
	}
	if cooldown == 0 {
		t.CooldownUntil = time.Time{}
	}
	return t, nil
}

// openTriggerThreadQuery is the upsert that reserves the key of t, and its arguments: the row is replaced only where
// store.TriggerThreadReplaceable says so.
func openTriggerThreadQuery(t store.TriggerThread, stale store.TriggerStale) (string, []any) {
	b := store.NewBinder(Dialect{})
	values := strings.Join([]string{b.Arg(t.Binding), b.Arg(t.KeyHash), b.Arg(t.Thread), b.Time(t.Opened), b.Time(t.Opened)}, ", ")
	replaceable := b.Render(store.TriggerThreadReplaceable(stale))
	return `INSERT INTO trigger_threads (` + triggerThreadColumns + `) VALUES (` + values + `, 0, 0)
			ON CONFLICT (binding, key_hash) DO UPDATE SET thread = excluded.thread, opened = excluded.opened,
				last_seen = excluded.last_seen, closed_at = 0, cooldown_until = 0
			WHERE ` + replaceable, b.Args()
}

// OpenTriggerThread implements store.TriggerThreads. The upsert's WHERE is the rule for replacing a row, and
// the write pool has one connection, so two starts of a key cannot both pass it. A new row has cooldown_until 0,
// which is a turn that has not started (MarkTriggerStarted).
func (s *adapter) OpenTriggerThread(ctx context.Context, t store.TriggerThread, stale store.TriggerStale) (store.TriggerThread, bool, error) {
	var current store.TriggerThread
	var opened bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		query, args := openTriggerThreadQuery(t, stale)
		n, err := execRows(ctx, tx, query, args...)
		if err != nil {
			return err
		}
		opened = n > 0
		current, err = scanTriggerThread(tx.QueryRowContext(ctx, `SELECT `+triggerThreadColumns+` FROM trigger_threads WHERE binding = ? AND key_hash = ?`, t.Binding, t.KeyHash))
		return err
	})
	if err != nil {
		return store.TriggerThread{}, false, fmt.Errorf("open trigger thread of %q: %w", t.Binding, err)
	}
	return current, opened, nil
}

// MarkTriggerStarted implements store.TriggerThreads.
func (s *adapter) MarkTriggerStarted(ctx context.Context, binding, keyHash, thread string, cooldownUntil time.Time) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE trigger_threads SET cooldown_until = ? WHERE binding = ? AND key_hash = ? AND thread = ?`, ts(cooldownUntil), binding, keyHash, thread)
		if err != nil || n > 0 {
			return err
		}
		var exists bool // no change: the row is missing, or another message's
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM trigger_threads WHERE binding = ? AND key_hash = ?)`, binding, keyHash).Scan(&exists); err != nil {
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
	n, err := s.exec(ctx, `DELETE FROM trigger_threads WHERE binding = ? AND key_hash = ? AND thread = ? AND `+store.TriggerThreadUnstarted("").Text(Dialect{}), binding, keyHash, thread)
	if err != nil {
		return false, fmt.Errorf("release trigger thread of %q: %w", binding, err)
	}
	return n > 0, nil
}

// GetTriggerThread implements store.TriggerThreads.
func (s *adapter) GetTriggerThread(ctx context.Context, binding, keyHash string) (store.TriggerThread, error) {
	t, err := scanTriggerThread(s.read.QueryRowContext(ctx, `SELECT `+triggerThreadColumns+` FROM trigger_threads WHERE binding = ? AND key_hash = ?`, binding, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		return store.TriggerThread{}, fmt.Errorf("get trigger thread of %q: %w", binding, store.ErrNotFound)
	}
	if err != nil {
		return store.TriggerThread{}, fmt.Errorf("get trigger thread of %q: %w", binding, err)
	}
	return t, nil
}

// ReopenTriggerThread implements store.TriggerThreads.
func (s *adapter) ReopenTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) error {
	n, err := s.exec(ctx, `UPDATE trigger_threads SET last_seen = MAX(last_seen, ?), closed_at = 0 WHERE binding = ? AND key_hash = ?`, ts(at), binding, keyHash)
	return errIfUnchanged(n, err, fmt.Sprintf("reopen trigger thread of %q", binding))
}

// CloseTriggerThread implements store.TriggerThreads.
func (s *adapter) CloseTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (bool, error) {
	var closed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE trigger_threads SET closed_at = ?, last_seen = MAX(last_seen, ?) WHERE binding = ? AND key_hash = ? AND `+store.TriggerThreadOpen("").Text(Dialect{}), ts(at), ts(at), binding, keyHash)
		if err != nil || n > 0 {
			closed = n > 0
			return err
		}
		var exists bool // no change: the row is missing, or closed already
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM trigger_threads WHERE binding = ? AND key_hash = ?)`, binding, keyHash).Scan(&exists); err != nil {
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
