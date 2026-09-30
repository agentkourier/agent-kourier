package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// EnqueueReply implements store.ReplyQueue.
func (s *Store) EnqueueReply(ctx context.Context, r store.Reply) (bool, error) {
	if r.ID == "" || !validKey(r.Key) || r.UserID == "" || r.MessageTS == "" || r.EnqueuedAt.IsZero() {
		return false, fmt.Errorf("enqueue reply %q: ID, key, UserID, MessageTS and EnqueuedAt are required: %w", r.ID, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, text, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		r.ID, r.Key.Connection, r.Key.Channel, r.Key.ThreadTS, r.UserID, r.MessageTS, r.Text, ts(r.EnqueuedAt))
	if err != nil {
		return false, fmt.Errorf("enqueue reply %q: %w", r.ID, err)
	}
	return n > 0, nil
}

// NextReply implements store.ReplyQueue.
func (s *Store) NextReply(ctx context.Context, key store.ConversationKey) (store.Reply, error) {
	if !validKey(key) {
		return store.Reply{}, fmt.Errorf("next reply %+v: key is required: %w", key, store.ErrInvalid)
	}
	r := store.Reply{Key: key}
	var enqueued int64
	err := s.read.QueryRowContext(ctx, `SELECT id, sequence, user_id, message_ts, text, enqueued_at FROM reply_queue
		WHERE connection = ? AND channel = ? AND thread_ts = ? AND delivered_at IS NULL ORDER BY sequence LIMIT 1`,
		key.Connection, key.Channel, key.ThreadTS).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageTS, &r.Text, &enqueued)
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
func (s *Store) MarkReplyDelivered(ctx context.Context, id string, at time.Time) error {
	if id == "" || at.IsZero() {
		return fmt.Errorf("mark reply %q delivered: ID and time are required: %w", id, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = COALESCE(delivered_at, ?), text = '' WHERE id = ?`, ts(at), id)
	return errIfUnchanged(n, err, fmt.Sprintf("mark reply %q delivered", id))
}
