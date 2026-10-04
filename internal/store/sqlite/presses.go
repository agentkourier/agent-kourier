package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// RecordPress implements store.Presses. The counts and the insert are one transaction on the
// single write connection, so the limits hold under concurrent presses.
func (s *Store) RecordPress(ctx context.Context, p store.Press, limits store.PressLimits) (telemetryResult0 int64, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.RecordPress")
	defer func() { finishStore(op, telemetryErr) }()
	if p.TraceParent == "" {
		p.TraceParent, p.TraceState = telemetry.Capture(ctx)
	}
	if p.InteractionID == "" || p.User == "" || p.Channel == "" || p.MessageRef == "" || p.PressedAt.IsZero() || limits.Total < 1 || limits.PerUser < 1 {
		return 0, fmt.Errorf("record press: the interaction, user, message, time and positive limits are required: %w", store.ErrInvalid)
	}
	if p.Inputs == nil {
		p.Inputs = []byte{} // a nil slice would bind as NULL
	}
	var seq int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var total, user int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(user_id = ?), 0) FROM interaction_presses`, p.User).Scan(&total, &user); err != nil {
			return fmt.Errorf("count presses: %w", err)
		}
		if total >= limits.Total || user >= limits.PerUser {
			return fmt.Errorf("%d presses wait, %d from %s: %w", total, user, p.User, store.ErrLimit)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO interaction_presses (interaction_id, user_id, channel, message_ts, inputs, pressed_at, trace_parent, trace_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, p.InteractionID, p.User, p.Channel, p.MessageRef, p.Inputs, ts(p.PressedAt), p.TraceParent, p.TraceState)
		if err != nil {
			return err
		}
		seq, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("record press of %s: %w", p.InteractionID, err)
	}
	return seq, nil
}

// ListPresses implements store.Presses.
func (s *Store) ListPresses(ctx context.Context) (telemetryResult0 []store.Press, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListPresses")
	defer func() { finishStore(op, telemetryErr) }()
	rows, err := s.read.QueryContext(ctx, `SELECT seq, interaction_id, user_id, channel, message_ts, inputs, pressed_at, trace_parent, trace_state
		FROM interaction_presses ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("list presses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.Press
	for rows.Next() {
		var p store.Press
		var at int64
		if err := rows.Scan(&p.Seq, &p.InteractionID, &p.User, &p.Channel, &p.MessageRef, &p.Inputs, &at, &p.TraceParent, &p.TraceState); err != nil {
			return nil, fmt.Errorf("list presses: %w", err)
		}
		p.PressedAt = fromTS(at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list presses: %w", err)
	}
	return out, nil
}

// DeletePress implements store.Presses.
func (s *Store) DeletePress(ctx context.Context, seq int64) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.DeletePress")
	defer func() { finishStore(op, telemetryErr) }()
	if _, err := s.exec(ctx, `DELETE FROM interaction_presses WHERE seq = ?`, seq); err != nil {
		return fmt.Errorf("delete press %d: %w", seq, err)
	}
	return nil
}

// PurgePresses implements store.Presses.
func (s *Store) PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (telemetryResult0 int, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PurgePresses")
	defer func() { finishStore(op, telemetryErr) }()
	if now.IsZero() || grace < 0 {
		return 0, fmt.Errorf("purge presses: now and a grace of at least zero are required: %w", store.ErrInvalid)
	}
	n, err := s.exec(ctx, `DELETE FROM interaction_presses WHERE seq IN (
		SELECT p.seq FROM interaction_presses p LEFT JOIN interactions i ON i.id = p.interaction_id
		WHERE COALESCE(i.deadline, p.pressed_at) + ? <= ?)`, grace.Nanoseconds(), ts(now))
	if err != nil {
		return 0, fmt.Errorf("purge presses: %w", err)
	}
	return int(n), nil
}

// PendingPresses implements store.Presses.
func (s *Store) PendingPresses(ctx context.Context, interactionID string, before time.Time) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PendingPresses")
	defer func() { finishStore(op, telemetryErr) }()
	if interactionID == "" || before.IsZero() {
		return false, fmt.Errorf("pending presses: an interaction and a time are required: %w", store.ErrInvalid)
	}
	var found bool
	err := s.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM interaction_presses WHERE interaction_id = ? AND pressed_at <= ?)`,
		interactionID, ts(before)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("pending presses of %s: %w", interactionID, err)
	}
	return found, nil
}
