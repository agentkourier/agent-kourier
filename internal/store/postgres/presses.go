package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// RecordPress implements store.Presses. The look-up of p's delivery key, the counts, over the presses of p's connection, and the
// inserts are one transaction under an advisory lock that every RecordPress takes, so the limits hold under concurrent presses:
// READ COMMITTED would let two callers count the same free place, and two deliveries of one press that come together could both
// find the key missing. The lock also makes the order of Seq the order of commits.
func (s *adapter) RecordPress(ctx context.Context, p store.Press, limits store.PressLimits) (int64, error) {
	var seq int64
	if p.Inputs == nil {
		p.Inputs = []byte{} // a nil slice would bind as NULL
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, lockPresses, ""); err != nil {
			return err
		}
		if p.Delivery != "" {
			var waiting *int64
			switch err := tx.QueryRow(ctx, `SELECT p.seq FROM press_deliveries d LEFT JOIN interaction_presses p ON p.seq = d.seq
				WHERE d.connection = $1 AND d.delivery_key = $2`, p.Connection, p.Delivery).Scan(&waiting); {
			case err == nil:
				if waiting != nil {
					seq = *waiting
				} // 0 once the press is processed
				return store.ErrDuplicate
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("look up the delivery: %w", err)
			}
		}
		var total, user int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE user_id = $1) FROM interaction_presses WHERE connection = $2`, p.User, p.Connection).Scan(&total, &user); err != nil {
			return fmt.Errorf("count presses: %w", err)
		}
		if total >= limits.Total || user >= limits.PerUser {
			return fmt.Errorf("%d presses wait, %d from %s: %w", total, user, p.User, store.ErrLimit)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO interaction_presses (interaction_id, user_id, connection, channel, message_ts, inputs, pressed_at, trace_parent, trace_state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING seq`,
			p.InteractionID, p.User, p.Connection, p.Channel, p.MessageRef, p.Inputs, p.PressedAt, p.TraceParent, p.TraceState).Scan(&seq); err != nil {
			return err
		}
		if p.Delivery == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO press_deliveries (connection, delivery_key, seq, pressed_at) VALUES ($1, $2, $3, $4)`, p.Connection, p.Delivery, seq, p.PressedAt)
		return err
	})
	if err != nil {
		if !errors.Is(err, store.ErrDuplicate) {
			seq = 0
		}
		return seq, fmt.Errorf("record press of %s: %w", p.InteractionID, err)
	}
	return seq, nil
}

// pressColumns are the columns of a press, in the order scanPress reads them.
const pressColumns = `seq, interaction_id, user_id, connection, channel, message_ts, inputs, pressed_at, trace_parent, trace_state`

func scanPress(r pgx.Row) (store.Press, error) {
	var p store.Press
	var at time.Time
	if err := r.Scan(&p.Seq, &p.InteractionID, &p.User, &p.Connection, &p.Channel, &p.MessageRef, &p.Inputs, &at, &p.TraceParent, &p.TraceState); err != nil {
		return store.Press{}, err
	}
	p.PressedAt = utc(at)
	return p, nil
}

// GetPress implements store.Presses.
func (s *adapter) GetPress(ctx context.Context, seq int64) (store.Press, error) {
	p, err := scanPress(s.pool.QueryRow(ctx, `SELECT `+pressColumns+` FROM interaction_presses WHERE seq = $1`, seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Press{}, fmt.Errorf("get press %d: %w", seq, store.ErrNotFound)
	} else if err != nil {
		return store.Press{}, fmt.Errorf("get press %d: %w", seq, err)
	}
	return p, nil
}

// ListPresses implements store.Presses.
func (s *adapter) ListPresses(ctx context.Context) ([]store.Press, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pressColumns+` FROM interaction_presses ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("list presses: %w", err)
	}
	defer rows.Close()
	var out []store.Press
	for rows.Next() {
		p, err := scanPress(rows)
		if err != nil {
			return nil, fmt.Errorf("list presses: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list presses: %w", err)
	}
	return out, nil
}

// DeletePress implements store.Presses. An identity column never gives a seq again, so a retried
// delete cannot remove a later press.
func (s *adapter) DeletePress(ctx context.Context, seq int64) error {
	if _, err := s.exec(ctx, `DELETE FROM interaction_presses WHERE seq = $1`, seq); err != nil {
		return fmt.Errorf("delete press %d: %w", seq, err)
	}
	return nil
}

// PurgePresses implements store.Presses. The presses and the delivery keys are deleted in one transaction.
func (s *adapter) PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (int, error) {
	var n int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM interaction_presses WHERE seq IN (
			SELECT p.seq FROM interaction_presses p LEFT JOIN interactions i ON i.id = p.interaction_id
			WHERE COALESCE(i.deadline, p.pressed_at) + $1::bigint * interval '1 microsecond' <= $2)`, grace.Microseconds(), now)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		_, err = tx.Exec(ctx, `DELETE FROM press_deliveries WHERE pressed_at <= $1`, now.Add(-grace))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("purge presses: %w", err)
	}
	return int(n), nil
}

// PendingPresses implements store.Presses.
func (s *adapter) PendingPresses(ctx context.Context, interactionID string, before time.Time) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM interaction_presses WHERE interaction_id = $1 AND pressed_at <= $2)
		OR EXISTS (SELECT 1 FROM reply_queue WHERE interaction_id = $1 AND delivered_at IS NULL AND enqueued_at <= $2)
		OR EXISTS (SELECT 1 FROM interactions i JOIN reply_queue r
		ON r.connection = i.connection AND r.channel = i.channel AND r.thread_ts = i.thread_ts
		WHERE i.id = $1 AND i.text_replies AND i.state = 'pending' AND i.message_ref <> ''
		AND r.interaction_id = '' AND r.user_id <> $3 AND r.delivered_at IS NULL AND r.enqueued_at >= i.created_at
		AND r.enqueued_at <= $2)`, interactionID, before, store.SystemUser).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("pending presses of %s: %w", interactionID, err)
	}
	return found, nil
}
