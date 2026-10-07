package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// RecordVote implements store.Feedback. The read of the person's earlier verdict and the write that follows are one
// transaction on the single write connection, so two votes of one person are applied one after the other.
func (s *adapter) RecordVote(ctx context.Context, v store.FeedbackVote) (store.Verdict, error) {
	var previous store.Verdict
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
func (s *adapter) ListVotes(ctx context.Context, key store.ConversationKey) ([]store.FeedbackVote, error) {
	var votes []store.FeedbackVote
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
