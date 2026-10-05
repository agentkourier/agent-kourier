package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// RecordPress implements store.Presses. The counts and the insert are one transaction under an advisory
// lock that every RecordPress takes, so the limits hold under concurrent presses: READ COMMITTED would
// let two callers count the same free place. The lock also makes the order of Seq the order of commits.
func (s *Store) RecordPress(ctx context.Context, p store.Press, limits store.PressLimits) (seq int64, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.RecordPress")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if p.TraceParent == "" {
		p.TraceParent, p.TraceState = telemetry.Capture(ctx)
	}
	if p.InteractionID == "" || p.User == "" || p.Channel == "" || p.MessageRef == "" || p.PressedAt.IsZero() || limits.Total < 1 || limits.PerUser < 1 {
		return 0, fmt.Errorf("record press: the interaction, user, message, time and positive limits are required: %w", store.ErrInvalid)
	}
	if p.Inputs == nil {
		p.Inputs = []byte{} // a nil slice would bind as NULL
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, lockPresses, ""); err != nil {
			return err
		}
		var total, user int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE user_id = $1) FROM interaction_presses`, p.User).Scan(&total, &user); err != nil {
			return fmt.Errorf("count presses: %w", err)
		}
		if total >= limits.Total || user >= limits.PerUser {
			return fmt.Errorf("%d presses wait, %d from %s: %w", total, user, p.User, store.ErrLimit)
		}
		return tx.QueryRow(ctx, `INSERT INTO interaction_presses (interaction_id, user_id, channel, message_ts, inputs, pressed_at, trace_parent, trace_state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING seq`,
			p.InteractionID, p.User, p.Channel, p.MessageRef, p.Inputs, p.PressedAt, p.TraceParent, p.TraceState).Scan(&seq)
	})
	if err != nil {
		return 0, fmt.Errorf("record press of %s: %w", p.InteractionID, err)
	}
	return seq, nil
}

// ListPresses implements store.Presses.
func (s *Store) ListPresses(ctx context.Context) (presses []store.Press, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListPresses")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	rows, err := s.pool.Query(ctx, `SELECT seq, interaction_id, user_id, channel, message_ts, inputs, pressed_at, trace_parent, trace_state
		FROM interaction_presses ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("list presses: %w", err)
	}
	defer rows.Close()
	var out []store.Press
	for rows.Next() {
		var p store.Press
		var at time.Time
		if err := rows.Scan(&p.Seq, &p.InteractionID, &p.User, &p.Channel, &p.MessageRef, &p.Inputs, &at, &p.TraceParent, &p.TraceState); err != nil {
			return nil, fmt.Errorf("list presses: %w", err)
		}
		p.PressedAt = utc(at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list presses: %w", err)
	}
	return out, nil
}

// DeletePress implements store.Presses. An identity column never gives a seq again, so a retried
// delete cannot remove a later press.
func (s *Store) DeletePress(ctx context.Context, seq int64) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.DeletePress")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if _, err := s.exec(ctx, `DELETE FROM interaction_presses WHERE seq = $1`, seq); err != nil {
		return fmt.Errorf("delete press %d: %w", seq, err)
	}
	return nil
}

// PurgePresses implements store.Presses.
func (s *Store) PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (purged int, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PurgePresses")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if now.IsZero() || grace < 0 {
		return 0, fmt.Errorf("purge presses: now and a grace of at least zero are required: %w", store.ErrInvalid)
	}
	n, err := s.exec(ctx, `DELETE FROM interaction_presses WHERE seq IN (
		SELECT p.seq FROM interaction_presses p LEFT JOIN interactions i ON i.id = p.interaction_id
		WHERE COALESCE(i.deadline, p.pressed_at) + $1::bigint * interval '1 microsecond' <= $2)`, grace.Microseconds(), now)
	if err != nil {
		return 0, fmt.Errorf("purge presses: %w", err)
	}
	return int(n), nil
}

// PendingPresses implements store.Presses.
func (s *Store) PendingPresses(ctx context.Context, interactionID string, before time.Time) (found bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PendingPresses")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if interactionID == "" || before.IsZero() {
		return false, fmt.Errorf("pending presses: an interaction and a time are required: %w", store.ErrInvalid)
	}
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM interaction_presses WHERE interaction_id = $1 AND pressed_at <= $2)
		OR EXISTS (SELECT 1 FROM reply_queue WHERE interaction_id = $1 AND delivered_at IS NULL AND enqueued_at <= $2)
		OR EXISTS (SELECT 1 FROM interactions i JOIN reply_queue r
		ON r.connection = i.connection AND r.channel = i.channel AND r.thread_ts = i.thread_ts
		WHERE i.id = $1 AND i.text_replies AND i.state = 'pending' AND i.message_ref <> ''
		AND r.interaction_id = '' AND r.delivered_at IS NULL AND r.enqueued_at >= i.created_at
		AND r.enqueued_at <= $2)`, interactionID, before).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("pending presses of %s: %w", interactionID, err)
	}
	return found, nil
}
