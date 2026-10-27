package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// EnqueueReply implements store.ReplyQueue.
//
// Within a session the enqueues run one at a time: the transaction takes the session's advisory lock
// before it inserts, and holds it until it commits. The Sequence of a reply is taken at the insert, so
// the reply that gets the next Sequence cannot start before the one before it is committed and shown,
// and a reader that walks the queue with NextReplyAfter never sees a Sequence without every lower one.
// A duplicate takes the lock too, and changes nothing.
func (s *adapter) EnqueueReply(ctx context.Context, r store.Reply) (bool, error) {
	var enqueued bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, lockReplyQueue, sessionLockKey(r.Key)); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, acknowledgment, text, enqueued_at, interaction_id, trace_parent, trace_state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (id) DO NOTHING`,
			r.ID, r.Key.Connection, r.Key.Channel, r.Key.Thread, r.UserID, r.MessageRef, r.Acknowledgment, r.Text, r.EnqueuedAt, r.InteractionID, r.TraceParent, r.TraceState)
		enqueued = tag.RowsAffected() > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("enqueue reply %q: %w", r.ID, err)
	}
	return enqueued, nil
}

// HasReply implements store.ReplyQueue.
func (s *adapter) HasReply(ctx context.Context, id string) (bool, error) {
	var found bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reply_queue WHERE id = $1)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("has reply %q: %w", id, err)
	}
	return found, nil
}

// HasReplies implements store.ReplyQueue: one statement for all the IDs, as an array that the unique index on id serves.
func (s *adapter) HasReplies(ctx context.Context, ids []string) (map[string]bool, error) {
	found := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM reply_queue WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("has replies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("has replies: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("has replies: %w", err)
	}
	return found, nil
}

// NextReply implements store.ReplyQueue.
func (s *adapter) NextReply(ctx context.Context, key store.ConversationKey) (store.Reply, error) {
	return s.nextReplyAfter(ctx, key, 0)
}

// NextReplyAfter implements store.ReplyQueue.
func (s *adapter) NextReplyAfter(ctx context.Context, key store.ConversationKey, after int64) (store.Reply, error) {
	return s.nextReplyAfter(ctx, key, after)
}

func (s *adapter) nextReplyAfter(ctx context.Context, key store.ConversationKey, after int64) (store.Reply, error) {
	r := store.Reply{Key: key}
	var enqueued time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, sequence, user_id, message_ts, acknowledgment, text, enqueued_at, interaction_id, trace_parent, trace_state FROM reply_queue
		WHERE connection = $1 AND channel = $2 AND thread_ts = $3 AND delivered_at IS NULL AND sequence > $4 ORDER BY sequence LIMIT 1`,
		key.Connection, key.Channel, key.Thread, after).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageRef, &r.Acknowledgment, &r.Text, &enqueued, &r.InteractionID, &r.TraceParent, &r.TraceState)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, err)
	}
	r.EnqueuedAt = utc(enqueued)
	return r, nil
}

// PreviousReply implements store.ReplyQueue.
func (s *adapter) PreviousReply(ctx context.Context, key store.ConversationKey, sequence int64) (store.Reply, error) {
	r := store.Reply{Key: key}
	var enqueued time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, sequence, user_id, message_ts, acknowledgment, text, enqueued_at, interaction_id, trace_parent, trace_state FROM reply_queue
		WHERE connection = $1 AND channel = $2 AND thread_ts = $3 AND sequence < $4 ORDER BY sequence DESC LIMIT 1`,
		key.Connection, key.Channel, key.Thread, sequence).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageRef, &r.Acknowledgment, &r.Text, &enqueued, &r.InteractionID, &r.TraceParent, &r.TraceState)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Reply{}, fmt.Errorf("previous reply %+v below %d: %w", key, sequence, store.ErrNotFound)
	}
	if err != nil {
		return store.Reply{}, fmt.Errorf("previous reply %+v below %d: %w", key, sequence, err)
	}
	r.EnqueuedAt = utc(enqueued)
	return r, nil
}

// MarkReplyDelivered implements store.ReplyQueue. COALESCE keeps the first delivery time, which makes
// a repeat a no-op that still matches the row.
func (s *adapter) MarkReplyDelivered(ctx context.Context, id string, at time.Time) error {
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = COALESCE(delivered_at, $1), text = '' WHERE id = $2`, at, id)
	return errIfUnchanged(n, err, fmt.Sprintf("mark reply %q delivered", id))
}

// ClaimReplyInteraction pins a queued answer to one question; retries cannot answer another.
func (s *adapter) ClaimReplyInteraction(ctx context.Context, id, interaction string) error {
	n, err := s.exec(ctx, `UPDATE reply_queue SET interaction_id = $1 WHERE id = $2 AND (interaction_id = '' OR interaction_id = $1)`, interaction, id)
	return errIfUnchanged(n, err, "claim reply interaction")
}

// AbandonStrandedReplies implements store.ReplyQueue. The session test is on the thread's row: a thread
// with no session has no runner that will ever send the reply, and one whose session records a task keeps
// the reply behind that task.
func (s *adapter) AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (int, error) {
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = $1, text = ''
		WHERE delivered_at IS NULL AND enqueued_at <= $2
		AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel
			AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '')`, now, now.Add(-age))
	if err != nil {
		return 0, fmt.Errorf("abandon stranded replies: %w", err)
	}
	return int(n), nil
}

// ListAcknowledgmentReplies implements store.AcknowledgmentLister. It returns
// delivered tombstones whose originating task is no longer active.
func (s *adapter) ListAcknowledgmentReplies(ctx context.Context, _ time.Time, _ time.Duration) ([]store.Reply, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, connection, channel, thread_ts, user_id, message_ts, acknowledgment, enqueued_at
		FROM reply_queue
		WHERE acknowledgment <> '' AND `+store.DeliveredReplies("").Text(Dialect{})+` AND NOT EXISTS (SELECT 1 FROM sessions s
			WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel
			AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '' AND s.turn_acknowledgment <> '' AND (s.turn_acknowledgment_id = reply_queue.id OR (s.turn_acknowledgment_id = '' AND s.turn_id = reply_queue.id)))`)
	if err != nil {
		return nil, fmt.Errorf("list acknowledgment replies: %w", err)
	}
	defer rows.Close()
	var replies []store.Reply
	for rows.Next() {
		var r store.Reply
		if err := rows.Scan(&r.ID, &r.Key.Connection, &r.Key.Channel, &r.Key.Thread, &r.UserID, &r.MessageRef, &r.Acknowledgment, &r.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("list acknowledgment replies: scan: %w", err)
		}
		r.EnqueuedAt = utc(r.EnqueuedAt)
		replies = append(replies, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list acknowledgment replies: %w", err)
	}
	return replies, nil
}

// SettleAcknowledgmentReply implements store.AcknowledgmentLister. A settled row still matches, so a repeat
// counts it and succeeds.
func (s *adapter) SettleAcknowledgmentReply(ctx context.Context, id string) error {
	n, err := s.exec(ctx, `UPDATE reply_queue SET acknowledgment = '' WHERE id = $1 AND `+store.DeliveredReplies("").Text(Dialect{}), id)
	return errIfUnchanged(n, err, fmt.Sprintf("settle acknowledgment reply %q", id))
}

// ListPendingAcknowledgmentReplies implements store.PendingAcknowledgmentLister.
func (s *adapter) ListPendingAcknowledgmentReplies(ctx context.Context) ([]store.Reply, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, connection, channel, thread_ts, user_id, message_ts, acknowledgment, enqueued_at
		FROM reply_queue
		WHERE acknowledgment <> '' AND (`+store.UndeliveredReplies("").Text(Dialect{})+` OR EXISTS (SELECT 1 FROM sessions s
			WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel
			AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '' AND s.turn_acknowledgment <> '' AND (s.turn_acknowledgment_id = reply_queue.id OR (s.turn_acknowledgment_id = '' AND s.turn_id = reply_queue.id))))`)
	if err != nil {
		return nil, fmt.Errorf("list pending acknowledgment replies: %w", err)
	}
	defer rows.Close()
	var replies []store.Reply
	for rows.Next() {
		var r store.Reply
		if err := rows.Scan(&r.ID, &r.Key.Connection, &r.Key.Channel, &r.Key.Thread, &r.UserID, &r.MessageRef, &r.Acknowledgment, &r.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("list pending acknowledgment replies: scan: %w", err)
		}
		r.EnqueuedAt = utc(r.EnqueuedAt)
		replies = append(replies, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending acknowledgment replies: %w", err)
	}
	return replies, nil
}
