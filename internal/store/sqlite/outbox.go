package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// CreateOutbox implements store.Outbox.
func (s *Store) CreateOutbox(ctx context.Context, o store.OutboxEntry) (bool, error) {
	if o.ID == "" || o.EventID == "" || o.Channel == "" || o.CreatedAt.IsZero() {
		return false, fmt.Errorf("create outbox %q: ID, EventID, Channel and CreatedAt are required: %w", o.ID, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		o.ID, o.EventID, o.Role, o.Channel, o.ThreadTS, ts(o.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("create outbox %q: %w", o.ID, err)
	}
	return n > 0, nil
}

// RecordMessageRef implements store.Outbox. The compare-and-set UPDATE and the
// read that tells a missing row from a conflict share one transaction, so the
// answer reflects the same state the UPDATE saw.
func (s *Store) RecordMessageRef(ctx context.Context, id, ref string) error {
	if id == "" || ref == "" {
		return fmt.Errorf("record message ref for outbox %q: ID and ref are required: %w", id, store.ErrInvalid)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE outbox SET message_ref = ? WHERE id = ? AND message_ref IN ('', ?)`, ref, id, ref)
		if err != nil || n > 0 {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT message_ref FROM outbox WHERE id = ?`, id).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return store.ErrNotFound
		case err != nil:
			return err
		case existing == ref:
			return nil
		}
		return fmt.Errorf("already posted as a different message: %w", store.ErrConflict)
	})
	if err != nil {
		return fmt.Errorf("record message ref for outbox %q: %w", id, err)
	}
	return nil
}

// ListUnposted implements store.Outbox.
func (s *Store) ListUnposted(ctx context.Context) ([]store.OutboxEntry, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, event_id, role, channel, thread_ts, created_at
		FROM outbox WHERE message_ref = '' ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list unposted outbox: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.OutboxEntry
	for rows.Next() {
		var o store.OutboxEntry
		var created int64
		if err := rows.Scan(&o.ID, &o.EventID, &o.Role, &o.Channel, &o.ThreadTS, &created); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		o.CreatedAt = fromTS(created)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unposted outbox: %w", err)
	}
	return out, nil
}
