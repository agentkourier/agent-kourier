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

// EnqueueReply implements store.ReplyQueue.
func (s *Store) EnqueueReply(ctx context.Context, r store.Reply) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.EnqueueReply")
	defer func() { finishStore(op, telemetryErr) }()
	if r.TraceParent == "" {
		r.TraceParent, r.TraceState = telemetry.Capture(ctx)
	}
	if r.ID == "" || !validKey(r.Key) || r.UserID == "" || r.MessageRef == "" || r.EnqueuedAt.IsZero() {
		return false, fmt.Errorf("enqueue reply %q: ID, key, UserID, MessageRef and EnqueuedAt are required: %w", r.ID, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, text, enqueued_at, trace_parent, trace_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		r.ID, r.Key.Connection, r.Key.Channel, r.Key.Thread, r.UserID, r.MessageRef, r.Text, ts(r.EnqueuedAt), r.TraceParent, r.TraceState)
	if err != nil {
		return false, fmt.Errorf("enqueue reply %q: %w", r.ID, err)
	}
	return n > 0, nil
}

// HasReply implements store.ReplyQueue.
func (s *Store) HasReply(ctx context.Context, id string) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.HasReply")
	defer func() { finishStore(op, telemetryErr) }()
	if id == "" {
		return false, fmt.Errorf("has reply: an ID is required: %w", store.ErrInvalid)
	}
	var found bool
	if err := s.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM reply_queue WHERE id = ?)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("has reply %q: %w", id, err)
	}
	return found, nil
}

// NextReply implements store.ReplyQueue.
func (s *Store) NextReply(ctx context.Context, key store.ConversationKey) (telemetryResult0 store.Reply, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.NextReply")
	defer func() { finishStore(op, telemetryErr) }()
	return s.nextReplyAfter(ctx, key, 0)
}

// NextReplyAfter implements store.ReplyQueue.
func (s *Store) NextReplyAfter(ctx context.Context, key store.ConversationKey, after int64) (result store.Reply, err error) {
	ctx, op := telemetry.Start(ctx, "courier.store.NextReplyAfter")
	defer func() { finishStore(op, err) }()
	return s.nextReplyAfter(ctx, key, after)
}

func (s *Store) nextReplyAfter(ctx context.Context, key store.ConversationKey, after int64) (store.Reply, error) {
	if !validKey(key) || after < 0 {
		return store.Reply{}, fmt.Errorf("next reply %+v: key is required: %w", key, store.ErrInvalid)
	}
	r := store.Reply{Key: key}
	var enqueued int64
	err := s.read.QueryRowContext(ctx, `SELECT id, sequence, user_id, message_ts, text, enqueued_at, interaction_id, trace_parent, trace_state FROM reply_queue
		WHERE connection = ? AND channel = ? AND thread_ts = ? AND delivered_at IS NULL AND sequence > ? ORDER BY sequence LIMIT 1`,
		key.Connection, key.Channel, key.Thread, after).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageRef, &r.Text, &enqueued, &r.InteractionID, &r.TraceParent, &r.TraceState)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, err)
	}
	r.EnqueuedAt = fromTS(enqueued)
	return r, nil
}

// MarkReplyDelivered implements store.ReplyQueue. COALESCE keeps the first
// delivery time, which makes a repeat a no-op that still matches the row.
func (s *Store) MarkReplyDelivered(ctx context.Context, id string, at time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.MarkReplyDelivered")
	defer func() { finishStore(op, telemetryErr) }()
	if id == "" || at.IsZero() {
		return fmt.Errorf("mark reply %q delivered: ID and time are required: %w", id, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = COALESCE(delivered_at, ?), text = '' WHERE id = ?`, ts(at), id)
	return errIfUnchanged(n, err, fmt.Sprintf("mark reply %q delivered", id))
}

// ClaimReplyInteraction pins a queued answer to one question; retries cannot answer another.
func (s *Store) ClaimReplyInteraction(ctx context.Context, id, interaction string) error {
	if id == "" || interaction == "" {
		return store.ErrInvalid
	}
	n, err := s.exec(ctx, `UPDATE reply_queue SET interaction_id = ? WHERE id = ? AND (interaction_id = '' OR interaction_id = ?)`, interaction, id, interaction)
	return errIfUnchanged(n, err, "claim reply interaction")
}
