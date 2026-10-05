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

// RecordVote implements store.Feedback. The read of the person's earlier verdict and the write that follows are one
// transaction on the single write connection, so two votes of one person are applied one after the other.
func (s *Store) RecordVote(ctx context.Context, v store.FeedbackVote) (previous store.Verdict, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.RecordVote")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(v.Key) || v.Turn == "" || v.User == "" || v.Binding == "" || v.Agent == "" || !v.Kind.Valid() || !v.Verdict.Valid() || !v.At.After(time.Unix(0, 0)) {
		return "", fmt.Errorf("record vote: the session, turn, user, Binding, agent, a kind, a verdict and a time after the Unix epoch are required: %w", store.ErrInvalid)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT verdict FROM feedback WHERE connection = ? AND channel = ? AND thread_ts = ? AND turn_id = ? AND user_id = ?`,
			v.Key.Connection, v.Key.Channel, v.Key.Thread, v.Turn, v.User).Scan(&previous)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO feedback (connection, channel, thread_ts, turn_id, user_id, binding, agent, alert_name, kind, verdict, first_at, voted_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				v.Key.Connection, v.Key.Channel, v.Key.Thread, v.Turn, v.User, v.Binding, v.Agent, v.Alert, string(v.Kind), string(v.Verdict), ts(v.At), ts(v.At))
			return err
		case err != nil:
			return err
		case previous == v.Verdict:
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE feedback SET verdict = ?, voted_at = ? WHERE connection = ? AND channel = ? AND thread_ts = ? AND turn_id = ? AND user_id = ?`,
			string(v.Verdict), ts(v.At), v.Key.Connection, v.Key.Channel, v.Key.Thread, v.Turn, v.User)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("record vote of %s on %s: %w", v.User, v.Turn, err)
	}
	return previous, nil
}

// ListVotes implements store.Feedback.
func (s *Store) ListVotes(ctx context.Context, key store.ConversationKey) (votes []store.FeedbackVote, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListVotes")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(key) {
		return nil, fmt.Errorf("list votes %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT turn_id, user_id, binding, agent, alert_name, kind, verdict, first_at, voted_at FROM feedback
		WHERE connection = ? AND channel = ? AND thread_ts = ? ORDER BY first_at, turn_id, user_id`, key.Connection, key.Channel, key.Thread)
	if err != nil {
		return nil, fmt.Errorf("list votes %+v: %w", key, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		v := store.FeedbackVote{Key: key}
		var first, last int64
		if err := rows.Scan(&v.Turn, &v.User, &v.Binding, &v.Agent, &v.Alert, &v.Kind, &v.Verdict, &first, &last); err != nil {
			return nil, fmt.Errorf("list votes %+v: %w", key, err)
		}
		v.First, v.At = fromTS(first), fromTS(last)
		votes = append(votes, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list votes %+v: %w", key, err)
	}
	return votes, nil
}
