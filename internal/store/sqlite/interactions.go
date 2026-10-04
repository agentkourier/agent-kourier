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

const interactionCols = `id, connection, channel, thread_ts, task_id, kind, state, request, message_ref,
	created_at, deadline, answer, decider, decided_at, delivered_at, trace_parent, trace_state`

// scanInteraction reads one row selected with interactionCols.
func scanInteraction(row interface{ Scan(...any) error }) (store.Interaction, error) {
	var i store.Interaction
	var created, deadline, decided, delivered int64
	err := row.Scan(&i.ID, &i.Key.Connection, &i.Key.Channel, &i.Key.Thread, &i.TaskID, &i.Kind, &i.State,
		&i.Request, &i.MessageRef, &created, &deadline, &i.Answer, &i.Decider, &decided, &delivered, &i.TraceParent, &i.TraceState)
	if err != nil {
		return store.Interaction{}, err
	}
	i.CreatedAt, i.Deadline = fromTS(created), fromTS(deadline)
	if decided != 0 {
		i.DecidedAt = fromTS(decided)
	}
	if delivered != 0 {
		i.DeliveredAt = fromTS(delivered)
	}
	return i, nil
}

// CreateInteraction implements store.Interactions.
func (s *Store) CreateInteraction(ctx context.Context, i store.Interaction) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.CreateInteraction")
	defer func() { finishStore(op, telemetryErr) }()
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
		created_at, deadline, trace_parent, trace_state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		i.ID, i.Key.Connection, i.Key.Channel, i.Key.Thread, i.TaskID, string(i.Kind), i.Request,
		ts(i.CreatedAt), ts(i.Deadline), i.TraceParent, i.TraceState)
	if err != nil {
		return false, fmt.Errorf("create interaction %q: %w", i.ID, err)
	}
	return n > 0, nil
}

// GetInteraction implements store.Interactions.
func (s *Store) GetInteraction(ctx context.Context, id string) (telemetryResult0 store.Interaction, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.GetInteraction")
	defer func() { finishStore(op, telemetryErr) }()
	i, err := scanInteraction(s.read.QueryRowContext(ctx, `SELECT `+interactionCols+` FROM interactions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Interaction{}, fmt.Errorf("get interaction %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.Interaction{}, fmt.Errorf("get interaction %q: %w", id, err)
	}
	return i, nil
}

// ListDueInteractions implements store.Interactions.
func (s *Store) ListDueInteractions(ctx context.Context, now time.Time) (telemetryResult0 []store.Interaction, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListDueInteractions")
	defer func() { finishStore(op, telemetryErr) }()
	if now.IsZero() {
		return nil, fmt.Errorf("list due interactions: now is required: %w", store.ErrInvalid)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT `+interactionCols+` FROM interactions
		WHERE state = 'pending' AND deadline <= ? ORDER BY deadline, id`, ts(now))
	if err != nil {
		return nil, fmt.Errorf("list due interactions: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.Interaction
	for rows.Next() {
		i, err := scanInteraction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan interaction row: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due interactions: %w", err)
	}
	return out, nil
}

// ListDecidedInteractions implements store.Interactions.
func (s *Store) ListDecidedInteractions(ctx context.Context, since time.Time) (telemetryResult0 []store.Interaction, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListDecidedInteractions")
	defer func() { finishStore(op, telemetryErr) }()
	if since.IsZero() {
		return nil, fmt.Errorf("list decided interactions: since is required: %w", store.ErrInvalid)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT `+interactionCols+` FROM interactions
		WHERE state IN ('answered', 'timed_out', 'cancelled') AND decided_at >= ? ORDER BY decided_at, id`, ts(since))
	if err != nil {
		return nil, fmt.Errorf("list decided interactions: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.Interaction
	for rows.Next() {
		i, err := scanInteraction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan interaction row: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decided interactions: %w", err)
	}
	return out, nil
}
