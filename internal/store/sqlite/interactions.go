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
	created_at, deadline, answer, decider, decided_at, delivered_at, trace_parent, trace_state, text_replies`

// scanInteraction reads one row selected with interactionCols.
func scanInteraction(row interface{ Scan(...any) error }) (store.Interaction, error) {
	var i store.Interaction
	var created, deadline, decided, delivered int64
	err := row.Scan(&i.ID, &i.Key.Connection, &i.Key.Channel, &i.Key.Thread, &i.TaskID, &i.Kind, &i.State,
		&i.Request, &i.MessageRef, &created, &deadline, &i.Answer, &i.Decider, &decided, &delivered, &i.TraceParent, &i.TraceState, &i.TextReplies)
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
func (s *adapter) CreateInteraction(ctx context.Context, i store.Interaction) (bool, error) {
	if i.Request == nil {
		i.Request = []byte{} // a nil slice would bind as NULL
	}
	n, err := s.exec(ctx, `INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, request,
		created_at, deadline, trace_parent, trace_state, text_replies) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		i.ID, i.Key.Connection, i.Key.Channel, i.Key.Thread, i.TaskID, string(i.Kind), i.Request,
		ts(i.CreatedAt), ts(i.Deadline), i.TraceParent, i.TraceState, i.TextReplies)
	if err != nil {
		return false, fmt.Errorf("create interaction %q: %w", i.ID, err)
	}
	return n > 0, nil
}

// GetInteraction implements store.Interactions.
func (s *adapter) GetInteraction(ctx context.Context, id string) (store.Interaction, error) {
	i, err := scanInteraction(s.read.QueryRowContext(ctx, `SELECT `+interactionCols+` FROM interactions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Interaction{}, fmt.Errorf("get interaction %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.Interaction{}, fmt.Errorf("get interaction %q: %w", id, err)
	}
	return i, nil
}

// PendingTextQuestion implements store.Interactions. interactions_thread serves the lookup.
func (s *adapter) PendingTextQuestion(ctx context.Context, key store.ConversationKey) (store.Interaction, error) {
	i, err := scanInteraction(s.read.QueryRowContext(ctx, `SELECT `+interactionCols+` FROM interactions
		WHERE connection = ? AND channel = ? AND thread_ts = ? AND state = 'pending' AND text_replies = 1
		ORDER BY created_at DESC, id DESC LIMIT 1`, key.Connection, key.Channel, key.Thread))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Interaction{}, fmt.Errorf("pending text question %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Interaction{}, fmt.Errorf("pending text question %+v: %w", key, err)
	}
	return i, nil
}

// ListDueInteractions implements store.Interactions.
func (s *adapter) ListDueInteractions(ctx context.Context, now time.Time) ([]store.Interaction, error) {
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
func (s *adapter) ListDecidedInteractions(ctx context.Context, since time.Time) ([]store.Interaction, error) {
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

// ClearSettledInteractions implements store.Interactions. A cleared row has empty blobs, so the
// length test is what keeps a second sweep from counting it again.
func (s *adapter) ClearSettledInteractions(ctx context.Context, now time.Time, grace, undeliveredGrace time.Duration) (int, error) {
	b := store.NewBinder(Dialect{})
	n, err := s.exec(ctx, `UPDATE interactions SET request = x'', answer = x'' WHERE `+b.Render(store.ClearableInteractions(now, grace, undeliveredGrace)), b.Args()...)
	if err != nil {
		return 0, fmt.Errorf("clear settled interactions: %w", err)
	}
	return int(n), nil
}
