package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// triggerTime reports whether t can be stored as one of a trigger thread's times: a stored 0 in closed_at
// means "open", so the zero time and anything at or before the Unix epoch are refused.
func triggerTime(t time.Time) bool { return !t.IsZero() && t.UnixNano() > 0 }

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

// OpenTriggerThread implements store.TriggerThreads. The upsert's WHERE is the rule for replacing a row, and
// the write pool has one connection, so two starts of a key cannot both pass it. A new row has cooldown_until 0,
// which is a turn that has not started (MarkTriggerStarted).
func (s *Store) OpenTriggerThread(ctx context.Context, t store.TriggerThread, stale store.TriggerStale) (telemetryResult0 store.TriggerThread, telemetryResult1 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.OpenTriggerThread")
	defer func() { finishStore(op, telemetryErr) }()
	if t.Binding == "" || t.KeyHash == "" || t.Thread == "" || !triggerTime(t.Opened) {
		return store.TriggerThread{}, false, fmt.Errorf("open trigger thread of %q: the Binding, key hash, thread and an opening time after the Unix epoch are required: %w", t.Binding, store.ErrInvalid)
	}
	var unstarted, idle int64 // no row was opened or seen before 0
	if triggerTime(stale.Unstarted) {
		unstarted = ts(stale.Unstarted)
	}
	if triggerTime(stale.Idle) {
		idle = ts(stale.Idle)
	}
	var current store.TriggerThread
	var opened bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `INSERT INTO trigger_threads (`+triggerThreadColumns+`) VALUES (?, ?, ?, ?, ?, 0, 0)
			ON CONFLICT (binding, key_hash) DO UPDATE SET thread = excluded.thread, opened = excluded.opened,
				last_seen = excluded.last_seen, closed_at = 0, cooldown_until = 0
			WHERE (trigger_threads.closed_at <> 0 AND trigger_threads.cooldown_until <= excluded.opened)
				OR (trigger_threads.cooldown_until = 0 AND trigger_threads.thread <> excluded.thread AND trigger_threads.opened < ?)
				OR (trigger_threads.closed_at = 0 AND trigger_threads.cooldown_until <> 0 AND trigger_threads.last_seen < ?)`,
			t.Binding, t.KeyHash, t.Thread, ts(t.Opened), ts(t.Opened), unstarted, idle)
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
func (s *Store) MarkTriggerStarted(ctx context.Context, binding, keyHash, thread string, cooldownUntil time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.MarkTriggerStarted")
	defer func() { finishStore(op, telemetryErr) }()
	if binding == "" || keyHash == "" || thread == "" || !triggerTime(cooldownUntil) {
		return fmt.Errorf("mark trigger thread of %q started: the Binding, key hash, thread and a cooldown end after the Unix epoch are required: %w", binding, store.ErrInvalid)
	}
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

// GetTriggerThread implements store.TriggerThreads.
func (s *Store) GetTriggerThread(ctx context.Context, binding, keyHash string) (telemetryResult0 store.TriggerThread, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.GetTriggerThread")
	defer func() { finishStore(op, telemetryErr) }()
	if binding == "" || keyHash == "" {
		return store.TriggerThread{}, fmt.Errorf("get trigger thread of %q: the Binding and key hash are required: %w", binding, store.ErrInvalid)
	}
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
func (s *Store) ReopenTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ReopenTriggerThread")
	defer func() { finishStore(op, telemetryErr) }()
	if binding == "" || keyHash == "" || !triggerTime(at) {
		return fmt.Errorf("reopen trigger thread of %q: the Binding, key hash and a time after the Unix epoch are required: %w", binding, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE trigger_threads SET last_seen = MAX(last_seen, ?), closed_at = 0 WHERE binding = ? AND key_hash = ?`, ts(at), binding, keyHash)
	return errIfUnchanged(n, err, fmt.Sprintf("reopen trigger thread of %q", binding))
}

// CloseTriggerThread implements store.TriggerThreads.
func (s *Store) CloseTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.CloseTriggerThread")
	defer func() { finishStore(op, telemetryErr) }()
	if binding == "" || keyHash == "" || !triggerTime(at) {
		return false, fmt.Errorf("close trigger thread of %q: the Binding, key hash and a time after the Unix epoch are required: %w", binding, store.ErrInvalid)
	}
	var closed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE trigger_threads SET closed_at = ?, last_seen = MAX(last_seen, ?) WHERE binding = ? AND key_hash = ? AND closed_at = 0`, ts(at), ts(at), binding, keyHash)
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
func (s *Store) PurgeTriggerThreads(ctx context.Context, now, idleBefore time.Time) (telemetryResult0 int, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PurgeTriggerThreads")
	defer func() { finishStore(op, telemetryErr) }()
	if !triggerTime(now) {
		return 0, fmt.Errorf("purge trigger threads: the time must be after the Unix epoch: %w", store.ErrInvalid)
	}
	var idle int64 // no row was seen before 0
	if triggerTime(idleBefore) {
		idle = ts(idleBefore)
	}
	n, err := s.exec(ctx, `DELETE FROM trigger_threads WHERE (closed_at <> 0 AND cooldown_until < ?) OR (closed_at = 0 AND last_seen < ?)`, ts(now), idle)
	if err != nil {
		return 0, fmt.Errorf("purge trigger threads: %w", err)
	}
	return int(n), nil
}
