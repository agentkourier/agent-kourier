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

// RecordVote implements store.Feedback. The person's row is read with FOR UPDATE and written in one transaction, so two
// votes of one person are applied one after the other. A first vote that another transaction inserted just before has no row
// to lock yet: the insert then does nothing, and the vote starts again from the read, which now finds the row.
func (s *Store) RecordVote(ctx context.Context, v store.FeedbackVote) (previous store.Verdict, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.RecordVote")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !validKey(v.Key) || v.Turn == "" || v.User == "" || v.Binding == "" || v.Agent == "" || !v.Kind.Valid() || !v.Verdict.Valid() || !afterEpoch(v.At) {
		return "", fmt.Errorf("record vote: the session, turn, user, Binding, agent, a kind, a verdict and a time after the Unix epoch are required: %w", store.ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for range 2 {
			previous = ""
			err := tx.QueryRow(ctx, `SELECT verdict FROM feedback WHERE connection = $1 AND channel = $2 AND thread_ts = $3 AND turn_id = $4 AND user_id = $5 FOR UPDATE`,
				v.Key.Connection, v.Key.Channel, v.Key.Thread, v.Turn, v.User).Scan(&previous)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				tag, err := tx.Exec(ctx, `INSERT INTO feedback (connection, channel, thread_ts, turn_id, user_id, binding, agent, alert_name, kind, verdict, first_at, voted_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11) ON CONFLICT DO NOTHING`,
					v.Key.Connection, v.Key.Channel, v.Key.Thread, v.Turn, v.User, v.Binding, v.Agent, v.Alert, string(v.Kind), string(v.Verdict), v.At)
				if err != nil || tag.RowsAffected() > 0 {
					return err
				}
				continue // a concurrent first vote won: this one is a change of it
			case err != nil:
				return err
			case previous == v.Verdict:
				return nil
			}
			_, err = tx.Exec(ctx, `UPDATE feedback SET verdict = $1, voted_at = $2 WHERE connection = $3 AND channel = $4 AND thread_ts = $5 AND turn_id = $6 AND user_id = $7`,
				string(v.Verdict), v.At, v.Key.Connection, v.Key.Channel, v.Key.Thread, v.Turn, v.User)
			return err
		}
		return errors.New("the row of a first vote was inserted and was not there to read")
	})
	if err != nil {
		return "", fmt.Errorf("record vote of %s on %s: %w", v.User, v.Turn, err)
	}
	return previous, nil
}

// ListVotes implements store.Feedback.
func (s *Store) ListVotes(ctx context.Context, key store.ConversationKey) (votes []store.FeedbackVote, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListVotes")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !validKey(key) {
		return nil, fmt.Errorf("list votes %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	rows, err := s.pool.Query(ctx, `SELECT turn_id, user_id, binding, agent, alert_name, kind, verdict, first_at, voted_at FROM feedback
		WHERE connection = $1 AND channel = $2 AND thread_ts = $3 ORDER BY first_at, turn_id, user_id`, key.Connection, key.Channel, key.Thread)
	if err != nil {
		return nil, fmt.Errorf("list votes %+v: %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		v := store.FeedbackVote{Key: key}
		var first, last time.Time
		if err := rows.Scan(&v.Turn, &v.User, &v.Binding, &v.Agent, &v.Alert, &v.Kind, &v.Verdict, &first, &last); err != nil {
			return nil, fmt.Errorf("list votes %+v: %w", key, err)
		}
		v.First, v.At = utc(first), utc(last)
		votes = append(votes, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list votes %+v: %w", key, err)
	}
	return votes, nil
}
