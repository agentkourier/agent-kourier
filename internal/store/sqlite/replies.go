package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// EnqueueReply implements store.ReplyQueue.
func (s *adapter) EnqueueReply(ctx context.Context, r store.Reply) (bool, error) {
	n, err := s.exec(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, acknowledgment, text, enqueued_at, interaction_id, trace_parent, trace_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		r.ID, r.Key.Connection, r.Key.Channel, r.Key.Thread, r.UserID, r.MessageRef, r.Acknowledgment, r.Text, ts(r.EnqueuedAt), r.InteractionID, r.TraceParent, r.TraceState)
	if err != nil {
		return false, fmt.Errorf("enqueue reply %q: %w", r.ID, err)
	}
	return n > 0, nil
}

// HasReply implements store.ReplyQueue.
func (s *adapter) HasReply(ctx context.Context, id string) (bool, error) {
	var found bool
	if err := s.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM reply_queue WHERE id = ?)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("has reply %q: %w", id, err)
	}
	return found, nil
}

// hasRepliesChunk is how many IDs one HasReplies statement carries: well under the 999 variables that older SQLite
// builds allow, and a longer list (up to store.MaxReplyIDs) is asked in several statements.
const hasRepliesChunk = 500

// HasReplies implements store.ReplyQueue: one statement for each chunk of the IDs, which the unique index on id serves.
func (s *adapter) HasReplies(ctx context.Context, ids []string) (map[string]bool, error) {
	found := make(map[string]bool, len(ids))
	for chunk := range slices.Chunk(ids, hasRepliesChunk) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		//nolint:gosec // G202: the text is "?" placeholders; every ID is an argument
		rows, err := s.read.QueryContext(ctx, `SELECT id FROM reply_queue WHERE id IN (?`+strings.Repeat(`, ?`, len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("has replies: %w", err)
		}
		if err := scanFound(rows, found); err != nil {
			return nil, fmt.Errorf("has replies: %w", err)
		}
	}
	return found, nil
}

// scanFound adds the IDs that rows holds to found, and closes rows.
func scanFound(rows *sql.Rows, found map[string]bool) error {
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	return rows.Err()
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
	var enqueued int64
	err := s.read.QueryRowContext(ctx, `SELECT id, sequence, user_id, message_ts, acknowledgment, text, enqueued_at, interaction_id, trace_parent, trace_state FROM reply_queue
		WHERE connection = ? AND channel = ? AND thread_ts = ? AND delivered_at IS NULL AND sequence > ? ORDER BY sequence LIMIT 1`,
		key.Connection, key.Channel, key.Thread, after).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageRef, &r.Acknowledgment, &r.Text, &enqueued, &r.InteractionID, &r.TraceParent, &r.TraceState)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, store.ErrNotFound)
	}
	if err != nil {
		return store.Reply{}, fmt.Errorf("next reply %+v: %w", key, err)
	}
	r.EnqueuedAt = fromTS(enqueued)
	return r, nil
}

// previousReplyQuery reads the row below a sequence from reply_queue_thread (migration 22), whose entries end in the rowid,
// which is the sequence: a seek on it, with no sort and no scan of the thread. TestPreviousReplyUsesTheThreadIndex holds it to that.
const previousReplyQuery = `SELECT id, sequence, user_id, message_ts, acknowledgment, text, enqueued_at, interaction_id, trace_parent, trace_state FROM reply_queue
	WHERE connection = ? AND channel = ? AND thread_ts = ? AND sequence < ? ORDER BY sequence DESC LIMIT 1`

// PreviousReply implements store.ReplyQueue.
func (s *adapter) PreviousReply(ctx context.Context, key store.ConversationKey, sequence int64) (store.Reply, error) {
	r := store.Reply{Key: key}
	var enqueued int64
	err := s.read.QueryRowContext(ctx, previousReplyQuery, key.Connection, key.Channel, key.Thread, sequence).Scan(&r.ID, &r.Sequence, &r.UserID, &r.MessageRef, &r.Acknowledgment, &r.Text, &enqueued, &r.InteractionID, &r.TraceParent, &r.TraceState)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Reply{}, fmt.Errorf("previous reply %+v below %d: %w", key, sequence, store.ErrNotFound)
	}
	if err != nil {
		return store.Reply{}, fmt.Errorf("previous reply %+v below %d: %w", key, sequence, err)
	}
	r.EnqueuedAt = fromTS(enqueued)
	return r, nil
}

// MarkReplyDelivered implements store.ReplyQueue. COALESCE keeps the first
// delivery time, which makes a repeat a no-op that still matches the row.
func (s *adapter) MarkReplyDelivered(ctx context.Context, id string, at time.Time) error {
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = COALESCE(delivered_at, ?), text = '' WHERE id = ?`, ts(at), id)
	return errIfUnchanged(n, err, fmt.Sprintf("mark reply %q delivered", id))
}

// ClaimReplyInteraction pins a queued answer to one question; retries cannot answer another.
func (s *adapter) ClaimReplyInteraction(ctx context.Context, id, interaction string) error {
	n, err := s.exec(ctx, `UPDATE reply_queue SET interaction_id = ? WHERE id = ? AND (interaction_id = '' OR interaction_id = ?)`, interaction, id, interaction)
	return errIfUnchanged(n, err, "claim reply interaction")
}

// AbandonStrandedReplies implements store.ReplyQueue. The session test is on the thread's row: a thread
// with no session has no runner that will ever send the reply, and one whose session records a task keeps
// the reply behind that task.
func (s *adapter) AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (int, error) {
	n, err := s.exec(ctx, `UPDATE reply_queue SET delivered_at = ?, text = ''
		WHERE delivered_at IS NULL AND enqueued_at <= ?
		AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel
			AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '')`, ts(now), ts(now.Add(-age)))
	if err != nil {
		return 0, fmt.Errorf("abandon stranded replies: %w", err)
	}
	return int(n), nil
}

// ListAcknowledgmentReplies implements store.AcknowledgmentLister. It returns
// delivered tombstones whose originating task is no longer active.
func (s *adapter) ListAcknowledgmentReplies(ctx context.Context, _ time.Time, _ time.Duration) ([]store.Reply, error) {
	//nolint:gosec // G202: the policy predicate is rendered by store.DeliveredReplies for this dialect.
	rows, err := s.read.QueryContext(ctx, `SELECT id, connection, channel, thread_ts, user_id, message_ts, acknowledgment, enqueued_at
		FROM reply_queue
		WHERE acknowledgment <> '' AND `+store.DeliveredReplies("").Text(Dialect{})+` AND NOT EXISTS (SELECT 1 FROM sessions s
			WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel
			AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '' AND s.turn_acknowledgment <> '' AND (s.turn_acknowledgment_id = reply_queue.id OR (s.turn_acknowledgment_id = '' AND s.turn_id = reply_queue.id)))`)
	if err != nil {
		return nil, fmt.Errorf("list acknowledgment replies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var replies []store.Reply
	for rows.Next() {
		var r store.Reply
		var enqueued int64
		if err := rows.Scan(&r.ID, &r.Key.Connection, &r.Key.Channel, &r.Key.Thread, &r.UserID, &r.MessageRef, &r.Acknowledgment, &enqueued); err != nil {
			return nil, fmt.Errorf("list acknowledgment replies: scan: %w", err)
		}
		r.EnqueuedAt = fromTS(enqueued)
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
	n, err := s.exec(ctx, `UPDATE reply_queue SET acknowledgment = '' WHERE id = ? AND `+store.DeliveredReplies("").Text(Dialect{}), id)
	return errIfUnchanged(n, err, fmt.Sprintf("settle acknowledgment reply %q", id))
}

// ListPendingAcknowledgmentReplies implements store.PendingAcknowledgmentLister.
func (s *adapter) ListPendingAcknowledgmentReplies(ctx context.Context) ([]store.Reply, error) {
	//nolint:gosec // G202: the policy predicate is rendered by store.UndeliveredReplies for this dialect.
	rows, err := s.read.QueryContext(ctx, `SELECT id, connection, channel, thread_ts, user_id, message_ts, acknowledgment, enqueued_at
		FROM reply_queue
		WHERE acknowledgment <> '' AND (`+store.UndeliveredReplies("").Text(Dialect{})+` OR EXISTS (SELECT 1 FROM sessions s
			WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel
			AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '' AND s.turn_acknowledgment <> '' AND (s.turn_acknowledgment_id = reply_queue.id OR (s.turn_acknowledgment_id = '' AND s.turn_id = reply_queue.id))))`)
	if err != nil {
		return nil, fmt.Errorf("list pending acknowledgment replies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var replies []store.Reply
	for rows.Next() {
		var r store.Reply
		var enqueued int64
		if err := rows.Scan(&r.ID, &r.Key.Connection, &r.Key.Channel, &r.Key.Thread, &r.UserID, &r.MessageRef, &r.Acknowledgment, &enqueued); err != nil {
			return nil, fmt.Errorf("list pending acknowledgment replies: scan: %w", err)
		}
		r.EnqueuedAt = fromTS(enqueued)
		replies = append(replies, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending acknowledgment replies: %w", err)
	}
	return replies, nil
}
