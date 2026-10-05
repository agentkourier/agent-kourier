package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

const interactionCols = `id, connection, channel, thread_ts, task_id, kind, state, request, message_ref,
	created_at, deadline, answer, decider, decided_at, delivered_at, trace_parent, trace_state, text_replies`

// scanInteraction reads one row selected with interactionCols.
func scanInteraction(row pgx.Row) (store.Interaction, error) {
	var i store.Interaction
	var created, deadline time.Time
	var decided, delivered *time.Time
	err := row.Scan(&i.ID, &i.Key.Connection, &i.Key.Channel, &i.Key.Thread, &i.TaskID, &i.Kind, &i.State,
		&i.Request, &i.MessageRef, &created, &deadline, &i.Answer, &i.Decider, &decided, &delivered, &i.TraceParent, &i.TraceState, &i.TextReplies)
	if err != nil {
		return store.Interaction{}, err
	}
	i.CreatedAt, i.Deadline = utc(created), utc(deadline)
	i.DecidedAt, i.DeliveredAt = fromNull(decided), fromNull(delivered)
	return i, nil
}

// CreateInteraction implements store.Interactions.
func (s *Store) CreateInteraction(ctx context.Context, i store.Interaction) (created bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.CreateInteraction")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if i.TraceParent == "" {
		i.TraceParent, i.TraceState = telemetry.Capture(ctx)
	}
	if i.ID == "" || !validKey(i.Key) || i.TaskID == "" || !i.Kind.Valid() ||
		i.CreatedAt.IsZero() || !i.Deadline.After(i.CreatedAt) {
		return false, fmt.Errorf("create interaction %q: ID, key, TaskID, Kind, CreatedAt and a later Deadline are required: %w", i.ID, store.ErrInvalid)
	}
	if i.Request == nil {
		i.Request = []byte{} // a nil slice would bind as NULL
	}
	n, err := s.exec(ctx, `INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, request,
		created_at, deadline, trace_parent, trace_state, text_replies) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (id) DO NOTHING`,
		i.ID, i.Key.Connection, i.Key.Channel, i.Key.Thread, i.TaskID, string(i.Kind), i.Request,
		i.CreatedAt, i.Deadline, i.TraceParent, i.TraceState, i.TextReplies)
	if err != nil {
		return false, fmt.Errorf("create interaction %q: %w", i.ID, err)
	}
	return n > 0, nil
}

// GetInteraction implements store.Interactions.
func (s *Store) GetInteraction(ctx context.Context, id string) (interaction store.Interaction, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.GetInteraction")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	i, err := scanInteraction(s.pool.QueryRow(ctx, `SELECT `+interactionCols+` FROM interactions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Interaction{}, fmt.Errorf("get interaction %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.Interaction{}, fmt.Errorf("get interaction %q: %w", id, err)
	}
	return i, nil
}

// listInteractions runs a query that selects interactionCols.
func (s *Store) listInteractions(ctx context.Context, what, query string, args ...any) ([]store.Interaction, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []store.Interaction
	for rows.Next() {
		i, err := scanInteraction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan interaction row: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// ListDueInteractions implements store.Interactions.
func (s *Store) ListDueInteractions(ctx context.Context, now time.Time) (interactions []store.Interaction, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListDueInteractions")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if now.IsZero() {
		return nil, fmt.Errorf("list due interactions: now is required: %w", store.ErrInvalid)
	}
	return s.listInteractions(ctx, "list due interactions", `SELECT `+interactionCols+` FROM interactions
		WHERE state = 'pending' AND deadline <= $1 ORDER BY deadline, id`, now)
}

// ListDecidedInteractions implements store.Interactions.
func (s *Store) ListDecidedInteractions(ctx context.Context, since time.Time) (interactions []store.Interaction, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListDecidedInteractions")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if since.IsZero() {
		return nil, fmt.Errorf("list decided interactions: since is required: %w", store.ErrInvalid)
	}
	return s.listInteractions(ctx, "list decided interactions", `SELECT `+interactionCols+` FROM interactions
		WHERE state IN ('answered', 'timed_out', 'cancelled') AND decided_at >= $1 ORDER BY decided_at, id`, since)
}

// transition runs update, which must be guarded by state = 'pending' and end in RETURNING kind, state,
// created_at, decided_at, and reports whether it changed the row. If it did not, the current state and
// decider are read in the same transaction, so the caller classifies the same state the update saw. A
// missing row returns ErrNotFound. Of concurrent transitions the row lock lets one UPDATE match; the
// others find the state it left.
func (s *Store) transition(ctx context.Context, id, update string, args ...any) (applied bool, state store.InteractionState, decider string, err error) {
	var kind string
	var created, decided time.Time
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var decidedAt *time.Time
		err := tx.QueryRow(ctx, update, args...).Scan(&kind, &state, &created, &decidedAt)
		if err == nil {
			applied, decided = true, fromNull(decidedAt)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT state, decider FROM interactions WHERE id = $1`, id).Scan(&state, &decider)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	})
	if applied && err == nil {
		telemetry.Observe(ctx, "courier_interaction_wait_seconds", decided.Sub(created), kind, string(state))
	}
	return applied, state, decider, err
}

// SubmitInteraction implements store.Interactions.
func (s *Store) SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.SubmitInteraction")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if id == "" || len(answer) == 0 || decider == "" || now.IsZero() {
		return fmt.Errorf("submit interaction %q: ID, answer, decider and now are required: %w", id, store.ErrInvalid)
	}
	applied, state, prev, err := s.transition(ctx, id, `UPDATE interactions SET state = 'answered', answer = $1, decider = $2,
		decided_at = $3 WHERE id = $4 AND state = 'pending' AND deadline > $3
		RETURNING kind, state, created_at, decided_at`, answer, decider, now, id)
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
func (s *Store) ResolveInteraction(ctx context.Context, id string, state store.InteractionState, now time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ResolveInteraction")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if id == "" || now.IsZero() || (state != store.InteractionTimedOut && state != store.InteractionCancelled) {
		return fmt.Errorf("resolve interaction %q to %q: ID, now and a timed_out or cancelled state are required: %w", id, state, store.ErrInvalid)
	}
	applied, cur, _, err := s.transition(ctx, id, `UPDATE interactions SET state = $1, decided_at = $2
		WHERE id = $3 AND state = 'pending' AND ($1 = 'cancelled' OR deadline <= $2)
		RETURNING kind, state, created_at, decided_at`, string(state), now, id)
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
func (s *Store) SetInteractionMessageRef(ctx context.Context, id, ref string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.SetInteractionMessageRef")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if id == "" || ref == "" {
		return fmt.Errorf("set message ref for interaction %q: ID and ref are required: %w", id, store.ErrInvalid)
	}
	if err := s.setMessageRef(ctx, interactionRefs, id, ref); err != nil {
		return fmt.Errorf("set message ref for interaction %q: %w", id, err)
	}
	return nil
}

// MarkInteractionDelivered implements store.Interactions.
func (s *Store) MarkInteractionDelivered(ctx context.Context, id string, at time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.MarkInteractionDelivered")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if id == "" || at.IsZero() {
		return fmt.Errorf("mark interaction %q delivered: an ID and a time are required: %w", id, store.ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE interactions SET delivered_at = $1 WHERE id = $2 AND state = 'answered' AND delivered_at IS NULL`, at, id)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		var state store.InteractionState // already marked, not answered, or not there
		err = tx.QueryRow(ctx, `SELECT state FROM interactions WHERE id = $1`, id).Scan(&state)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return store.ErrNotFound
		case err != nil:
			return err
		case state != store.InteractionAnswered:
			return fmt.Errorf("it is %s, not answered: %w", state, store.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mark interaction %q delivered: %w", id, err)
	}
	return nil
}
