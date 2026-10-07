package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// eventColumns are the columns scanEvent reads.
const eventColumns = `id, kind, binding_key, payload, received_at, status, attempts, next_attempt_at, last_error, trace_parent, trace_state`

// scanEvent reads one row selected with eventColumns.
func scanEvent(r pgx.Row) (store.Event, error) {
	var e store.Event
	var received, next time.Time
	if err := r.Scan(&e.ID, &e.Kind, &e.BindingKey, &e.Payload, &received, &e.Status, &e.Attempts, &next, &e.LastError, &e.TraceParent, &e.TraceState); err != nil {
		return store.Event{}, err
	}
	e.ReceivedAt, e.NextAttemptAt = utc(received), utc(next)
	return e, nil
}

// AppendEvent implements store.Events.
func (s *adapter) AppendEvent(ctx context.Context, e store.Event) (bool, error) {
	if e.Payload == nil {
		e.Payload = []byte{} // a nil slice would bind as NULL
	}
	n, err := s.exec(ctx, `INSERT INTO events (id, kind, binding_key, payload, received_at, next_attempt_at, trace_parent, trace_state)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7) ON CONFLICT (id) DO NOTHING`,
		e.ID, e.Kind, e.BindingKey, e.Payload, e.ReceivedAt, e.TraceParent, e.TraceState)
	if err != nil {
		return false, fmt.Errorf("append event %q: %w", e.ID, err)
	}
	return n > 0, nil
}

// GetEvent implements store.Events.
func (s *adapter) GetEvent(ctx context.Context, id string) (store.Event, error) {
	e, err := scanEvent(s.pool.QueryRow(ctx, `SELECT `+eventColumns+` FROM events WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, fmt.Errorf("get event %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.Event{}, fmt.Errorf("get event %q: %w", id, err)
	}
	return e, nil
}

// SetEventStatus implements store.Events.
func (s *adapter) SetEventStatus(ctx context.Context, id string, status store.EventStatus, lastError string) error {
	n, err := s.exec(ctx, `UPDATE events SET status = $1, last_error = $2 WHERE id = $3`, string(status), lastError, id)
	return errIfUnchanged(n, err, fmt.Sprintf("set event %q status", id))
}

// ClaimEvent implements store.Queue.
//
// The claim is one UPDATE whose candidate is picked by a subquery that locks it with FOR UPDATE SKIP
// LOCKED: a claimer that meets a row another claimer holds passes it by and takes the next, so
// claimers never wait for one another and never take one event twice. While an event is processing,
// next_attempt_at holds its lease deadline, which lets one predicate cover both due and expired events,
// and a lease that expired is taken over by exactly one claimer, as the row lock lets only one through.
func (s *adapter) ClaimEvent(ctx context.Context, now time.Time, lease time.Duration) (store.Event, error) {
	var e store.Event
	err := s.inTx(ctx, func(tx pgx.Tx) (err error) {
		e, err = scanEvent(tx.QueryRow(ctx, `UPDATE events
			SET status = 'processing', attempts = attempts + 1, next_attempt_at = $1
			WHERE id = (SELECT id FROM events
				WHERE status IN ('pending', 'processing') AND next_attempt_at <= $2
				ORDER BY next_attempt_at, received_at LIMIT 1
				FOR UPDATE SKIP LOCKED)
			RETURNING `+eventColumns, now.Add(lease), now))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, fmt.Errorf("claim event: %w", store.ErrNotFound)
	}
	if err != nil {
		return store.Event{}, fmt.Errorf("claim event: %w", err)
	}
	return e, nil
}

// CompleteEvent implements store.Queue.
func (s *adapter) CompleteEvent(ctx context.Context, id string, attempt int) error {
	return s.fenced(ctx, id, attempt, "complete", `UPDATE events SET status = 'done' WHERE id = $1 AND status = 'processing' AND attempts = $2`, id, attempt)
}

// FailEvent implements store.Queue. The fence pins the row's attempts to attempt, so the attempts >= $2
// test compares the caller's own attempt.
func (s *adapter) FailEvent(ctx context.Context, id string, attempt int, retryAt time.Time, maxAttempts int, errText string) error {
	return s.fenced(ctx, id, attempt, "fail", `UPDATE events SET last_error = $1,
			status = CASE WHEN attempts >= $2 THEN 'dead' ELSE 'pending' END,
			next_attempt_at = CASE WHEN attempts >= $2 THEN next_attempt_at ELSE $3 END
		WHERE id = $4 AND status = 'processing' AND attempts = $5`,
		errText, maxAttempts, retryAt, id, attempt)
}

// fenced runs update, which must be guarded by the claim's attempt, and turns "no rows changed" into
// ErrNotFound for a missing event or ErrConflict for one the caller no longer holds. The update and
// the read that tells them apart share a transaction, so the answer is about the state the update saw.
func (s *adapter) fenced(ctx context.Context, id string, attempt int, op, update string, args ...any) error {
	what := fmt.Sprintf("%s event %q", op, id)
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, update, args...)
		if err != nil || tag.RowsAffected() > 0 {
			return errIfUnchanged(tag.RowsAffected(), err, what)
		}
		var status store.EventStatus
		var attempts int
		err = tx.QueryRow(ctx, `SELECT status, attempts FROM events WHERE id = $1`, id).Scan(&status, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: %w", what, store.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return fmt.Errorf("%s: lease lost: event is %s at attempt %d, caller holds %d: %w", what, status, attempts, attempt, store.ErrConflict)
	})
}
