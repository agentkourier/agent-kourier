package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const interactionCols = `id, connection, channel, thread_ts, task_id, kind, state, request, message_ref,
	created_at, deadline, answer, decider, decided_at`

// scanInteraction reads one row selected with interactionCols.
func scanInteraction(row interface{ Scan(...any) error }) (store.Interaction, error) {
	var i store.Interaction
	var created, deadline, decided int64
	err := row.Scan(&i.ID, &i.Key.Connection, &i.Key.Channel, &i.Key.ThreadTS, &i.TaskID, &i.Kind, &i.State,
		&i.Request, &i.MessageRef, &created, &deadline, &i.Answer, &i.Decider, &decided)
	if err != nil {
		return store.Interaction{}, err
	}
	i.CreatedAt, i.Deadline = fromTS(created), fromTS(deadline)
	if decided != 0 {
		i.DecidedAt = fromTS(decided)
	}
	return i, nil
}

// CreateInteraction implements store.Interactions.
func (s *Store) CreateInteraction(ctx context.Context, i store.Interaction) (bool, error) {
	if i.ID == "" || !validKey(i.Key) || i.TaskID == "" || !i.Kind.Valid() ||
		i.CreatedAt.IsZero() || !i.Deadline.After(i.CreatedAt) {
		return false, fmt.Errorf("create interaction %q: ID, key, TaskID, Kind, CreatedAt and a later Deadline are required: %w", i.ID, store.ErrInvalid)
	}
	if i.Request == nil {
		i.Request = []byte{} // a nil slice would bind as NULL
	}
	n, err := s.exec(ctx, `INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, request,
		created_at, deadline) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		i.ID, i.Key.Connection, i.Key.Channel, i.Key.ThreadTS, i.TaskID, string(i.Kind), i.Request,
		ts(i.CreatedAt), ts(i.Deadline))
	if err != nil {
		return false, fmt.Errorf("create interaction %q: %w", i.ID, err)
	}
	return n > 0, nil
}

// GetInteraction implements store.Interactions.
func (s *Store) GetInteraction(ctx context.Context, id string) (store.Interaction, error) {
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
func (s *Store) ListDueInteractions(ctx context.Context, now time.Time) ([]store.Interaction, error) {
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
