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

// EnqueueReply implements store.ReplyQueue.
//
// Within a session the enqueues run one at a time: the transaction takes the session's advisory lock
// before it inserts, and holds it until it commits. The Sequence of a reply is taken at the insert, so
// the reply that gets the next Sequence cannot start before the one before it is committed and shown,
// and a reader that walks the queue with NextReplyAfter never sees a Sequence without every lower one.
// A duplicate takes the lock too, and changes nothing.
func (s *Store) EnqueueReply(ctx context.Context, r store.Reply) (enqueued bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.EnqueueReply")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if r.TraceParent == "" {
		r.TraceParent, r.TraceState = telemetry.Capture(ctx)
	}
	if r.ID == "" || !validKey(r.Key) || r.UserID == "" || r.MessageRef == "" || r.EnqueuedAt.IsZero() {
		return false, fmt.Errorf("enqueue reply %q: ID, key, UserID, MessageRef and EnqueuedAt are required: %w", r.ID, store.ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, lockReplyQueue, sessionLockKey(r.Key)); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, text, enqueued_at, trace_parent, trace_state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO NOTHING`,
			r.ID, r.Key.Connection, r.Key.Channel, r.Key.Thread, r.UserID, r.MessageRef, r.Text, r.EnqueuedAt, r.TraceParent, r.TraceState)
		enqueued = tag.RowsAffected() > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("enqueue reply %q: %w", r.ID, err)
	}
	return enqueued, nil
}

// HasReply implements store.ReplyQueue.
func (s *Store) HasReply(ctx context.Context, id string) (found bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.HasReply")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if id == "" {
		return false, fmt.Errorf("has reply: an ID is required: %w", store.ErrInvalid)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reply_queue WHERE id = $1)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("has reply %q: %w", id, err)
	}
	return found, nil
}

// NextReply implements store.ReplyQueue.
func (s *Store) NextReply(ctx context.Context, key store.ConversationKey) (reply store.Reply, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.NextReply")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	return s.nextReplyAfter(ctx, key, 0)
}

// NextReplyAfter implements store.ReplyQueue.
func (s *Store) NextReplyAfter(ctx context.Context, key store.ConversationKey, after int64) (reply store.Reply, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.NextReplyAfter")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	return s.nextReplyAfter(ctx, key, after)
}

func (s *Store) nextReplyAfter(ctx context.Context, key store.ConversationKey, after int64) (store.Reply, error) {
	if !validKey(key) || after < 0 {
		return store.Reply{}, fmt.Errorf("next reply %+v: key is required: %w", key, store.ErrInvalid)
	}
	r := store.Reply{Key: key}
	var enqueued time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, sequence, user_id, message_ts, text, enqueued_at, interaction_id, trace_parent, trace_state FROM reply_queue
		WHERE connection = $1 AND channel = $2 AND thread_ts = $3 AND delivered_at IS NULL AND sequence > $4 ORDER BY sequence LIMIT 1`,
		key.Connection, key.Channel, key.Thread, after).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageRef, &r.Text, &enqueued, &r.InteractionID, &r.TraceParent, &r.TraceState)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, err)
	}
	r.EnqueuedAt = utc(enqueued)
	return r, nil
}

// MarkReplyDelivered implements store.ReplyQueue. COALESCE keeps the first delivery time, which makes
// a repeat a no-op that still matches the row.
func (s *Store) MarkReplyDelivered(ctx context.Context, id string, at time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.MarkReplyDelivered")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if id == "" || at.IsZero() {
		return fmt.Errorf("mark reply %q delivered: ID and time are required: %w", id, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = COALESCE(delivered_at, $1), text = '' WHERE id = $2`, at, id)
	return errIfUnchanged(n, err, fmt.Sprintf("mark reply %q delivered", id))
}

// ClaimReplyInteraction pins a queued answer to one question; retries cannot answer another.
func (s *Store) ClaimReplyInteraction(ctx context.Context, id, interaction string) (telemetryErr error) {
	defer func() { telemetryErr = s.red.scrub(telemetryErr) }()
	if id == "" || interaction == "" {
		return store.ErrInvalid
	}
	n, err := s.exec(ctx, `UPDATE reply_queue SET interaction_id = $1 WHERE id = $2 AND (interaction_id = '' OR interaction_id = $1)`, interaction, id)
	return errIfUnchanged(n, err, "claim reply interaction")
}
