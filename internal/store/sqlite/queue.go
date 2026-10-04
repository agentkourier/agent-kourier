package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// ClaimEvent implements store.Queue.
//
// The claim is one UPDATE ... RETURNING on the single-connection write pool,
// so selecting the candidate and marking it processing cannot interleave with
// another claimer. While an event is processing, next_attempt_at holds its
// lease deadline, which lets one predicate cover both due and expired events.
func (s *Store) ClaimEvent(ctx context.Context, now time.Time, lease time.Duration) (telemetryResult0 store.Event, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ClaimEvent")
	defer func() { finishStore(op, telemetryErr) }()
	if now.IsZero() || lease <= 0 {
		return store.Event{}, fmt.Errorf("claim event: now must be set and lease positive: %w", store.ErrInvalid)
	}
	var e store.Event
	var received, next int64
	err := s.write.QueryRowContext(ctx, `UPDATE events
		SET status = 'processing', attempts = attempts + 1, next_attempt_at = ?
		WHERE id = (SELECT id FROM events
			WHERE status IN ('pending', 'processing') AND next_attempt_at <= ?
			ORDER BY next_attempt_at, received_at LIMIT 1)
		RETURNING id, kind, binding_key, payload, received_at, status, attempts, next_attempt_at, last_error, trace_parent, trace_state`,
		ts(now.Add(lease)), ts(now)).
		Scan(&e.ID, &e.Kind, &e.BindingKey, &e.Payload, &received, &e.Status, &e.Attempts, &next, &e.LastError, &e.TraceParent, &e.TraceState)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Event{}, fmt.Errorf("claim event: %w", store.ErrNotFound)
	}
	if err != nil {
		return store.Event{}, fmt.Errorf("claim event: %w", err)
	}
	e.ReceivedAt, e.NextAttemptAt = fromTS(received), fromTS(next)
	return e, nil
}

// CompleteEvent implements store.Queue.
func (s *Store) CompleteEvent(ctx context.Context, id string, attempt int) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.CompleteEvent")
	defer func() { finishStore(op, telemetryErr) }()
	if id == "" || attempt < 1 {
		return fmt.Errorf("complete event %q: ID and attempt are required: %w", id, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE events SET status = 'done' WHERE id = ? AND status = 'processing' AND attempts = ?`,
		id, attempt)
	return s.errIfLeaseLost(ctx, n, err, id, attempt, "complete")
}

// FailEvent implements store.Queue. The fence pins the row's attempts to
// attempt, so the attempts >= ? test compares the caller's own attempt.
func (s *Store) FailEvent(ctx context.Context, id string, attempt int, retryAt time.Time, maxAttempts int, errText string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.FailEvent")
	defer func() { finishStore(op, telemetryErr) }()
	if id == "" || attempt < 1 || retryAt.IsZero() || maxAttempts < 1 {
		return fmt.Errorf("fail event %q: ID and retryAt are required and attempt and maxAttempts must be at least 1: %w",
			id, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE events SET last_error = ?,
			status = CASE WHEN attempts >= ? THEN 'dead' ELSE 'pending' END,
			next_attempt_at = CASE WHEN attempts >= ? THEN next_attempt_at ELSE ? END
		WHERE id = ? AND status = 'processing' AND attempts = ?`,
		errText, maxAttempts, maxAttempts, ts(retryAt), id, attempt)
	return s.errIfLeaseLost(ctx, n, err, id, attempt, "fail")
}

// errIfLeaseLost turns "no rows changed" from a fenced update into ErrNotFound
// for a missing event or ErrConflict for one the caller no longer holds.
func (s *Store) errIfLeaseLost(ctx context.Context, n int64, err error, id string, attempt int, op string) error {
	what := fmt.Sprintf("%s event %q", op, id)
	if err != nil || n > 0 {
		return errIfUnchanged(n, err, what)
	}
	e, err := s.GetEvent(ctx, id)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: lease lost: event is %s at attempt %d, caller holds %d: %w",
		what, e.Status, e.Attempts, attempt, store.ErrConflict)
}
