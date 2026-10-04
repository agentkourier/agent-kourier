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
	return t, nil
}

// OpenTriggerThread implements store.TriggerThreads. The upsert's WHERE is the rule for replacing a row, and
// the write pool has one connection, so two starts of a key cannot both pass it.
func (s *Store) OpenTriggerThread(ctx context.Context, t store.TriggerThread) (telemetryResult0 store.TriggerThread, telemetryResult1 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.OpenTriggerThread")
	defer func() { finishStore(op, telemetryErr) }()
	if t.Binding == "" || t.KeyHash == "" || t.Thread == "" || !triggerTime(t.Opened) || t.CooldownUntil.Before(t.Opened) {
		return store.TriggerThread{}, false, fmt.Errorf("open trigger thread of %q: the Binding, key hash, thread and opening time are required, and the cooldown may not end before it: %w", t.Binding, store.ErrInvalid)
	}
	var current store.TriggerThread
	var opened bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `INSERT INTO trigger_threads (`+triggerThreadColumns+`) VALUES (?, ?, ?, ?, ?, 0, ?)
			ON CONFLICT (binding, key_hash) DO UPDATE SET thread = excluded.thread, opened = excluded.opened,
				last_seen = excluded.last_seen, closed_at = 0, cooldown_until = excluded.cooldown_until
			WHERE trigger_threads.closed_at <> 0 AND trigger_threads.cooldown_until <= excluded.opened`,
			t.Binding, t.KeyHash, t.Thread, ts(t.Opened), ts(t.Opened), ts(t.CooldownUntil))
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
