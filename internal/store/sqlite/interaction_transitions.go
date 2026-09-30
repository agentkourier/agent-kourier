package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// transition runs update, which must be guarded by state = 'pending', and
// reports whether it changed the row. If it did not, the current state and
// decider are read in the same transaction, so the caller classifies the same
// state the update saw. A missing row returns ErrNotFound.
func (s *Store) transition(ctx context.Context, id, update string, args ...any) (applied bool, state store.InteractionState, decider string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, update, args...)
		if err != nil || n > 0 {
			applied = n > 0
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT state, decider FROM interactions WHERE id = ?`, id).Scan(&state, &decider)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	})
	return applied, state, decider, err
}

// SubmitInteraction implements store.Interactions.
func (s *Store) SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) error {
	if id == "" || len(answer) == 0 || decider == "" || now.IsZero() {
		return fmt.Errorf("submit interaction %q: ID, answer, decider and now are required: %w", id, store.ErrInvalid)
	}
	applied, state, prev, err := s.transition(ctx, id, `UPDATE interactions SET state = 'answered', answer = ?, decider = ?,
		decided_at = ? WHERE id = ? AND state = 'pending' AND deadline > ?`, answer, decider, ts(now), id, ts(now))
	switch {
	case err != nil:
		return fmt.Errorf("submit interaction %q: %w", id, err)
	case applied:
		return nil
	case state == store.InteractionPending || state == store.InteractionTimedOut:
		return fmt.Errorf("submit interaction %q: past its deadline: %w", id, store.ErrExpired)
	case prev != "":
		return fmt.Errorf("submit interaction %q: already %s by %s: %w", id, state, prev, store.ErrConflict)
	}
	return fmt.Errorf("submit interaction %q: already %s: %w", id, state, store.ErrConflict)
}

// ResolveInteraction implements store.Interactions.
func (s *Store) ResolveInteraction(ctx context.Context, id string, state store.InteractionState, now time.Time) error {
	if id == "" || now.IsZero() || (state != store.InteractionTimedOut && state != store.InteractionCancelled) {
		return fmt.Errorf("resolve interaction %q to %q: ID, now and a timed_out or cancelled state are required: %w", id, state, store.ErrInvalid)
	}
	applied, cur, _, err := s.transition(ctx, id, `UPDATE interactions SET state = ?, decided_at = ?
		WHERE id = ? AND state = 'pending' AND (? = 'cancelled' OR deadline <= ?)`, string(state), ts(now), id, string(state), ts(now))
	switch {
	case err != nil:
		return fmt.Errorf("resolve interaction %q: %w", id, err)
	case cur == store.InteractionPending: // not applied, so only a timeout before the deadline
		return fmt.Errorf("resolve interaction %q: not yet due: %w", id, store.ErrConflict)
	case !applied:
		return fmt.Errorf("resolve interaction %q: already %s: %w", id, cur, store.ErrConflict)
	}
	return nil
}

// SetInteractionMessageRef implements store.Interactions.
func (s *Store) SetInteractionMessageRef(ctx context.Context, id, ref string) error {
	if id == "" || ref == "" {
		return fmt.Errorf("set message ref for interaction %q: ID and ref are required: %w", id, store.ErrInvalid)
	}
	if err := s.setMessageRef(ctx, "interactions", id, ref); err != nil {
		return fmt.Errorf("set message ref for interaction %q: %w", id, err)
	}
	return nil
}
