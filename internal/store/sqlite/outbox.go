package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// CreateOutbox implements store.Outbox.
func (s *Store) CreateOutbox(ctx context.Context, o store.OutboxEntry) (bool, error) {
	if o.ID == "" || o.EventID == "" || o.Channel == "" || o.CreatedAt.IsZero() || !o.Role.Valid() {
		return false, fmt.Errorf("create outbox %q: ID, EventID, a valid Role, Channel and CreatedAt are required: %w", o.ID, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		o.ID, o.EventID, o.Role, o.Channel, o.ThreadTS, ts(o.CreatedAt), o.Payload)
	if err != nil {
		return false, fmt.Errorf("create outbox %q: %w", o.ID, err)
	}
	return n > 0, nil
}

// RecordMessageRef implements store.Outbox.
func (s *Store) RecordMessageRef(ctx context.Context, id, ref string) error {
	if id == "" || ref == "" {
		return fmt.Errorf("record message ref for outbox %q: ID and ref are required: %w", id, store.ErrInvalid)
	}
	if err := s.setMessageRef(ctx, outboxRefs, id, ref); err != nil {
		return fmt.Errorf("record message ref for outbox %q: %w", id, err)
	}
	return nil
}

// outboxColumns are the columns scanOutbox reads.
const outboxColumns = `id, event_id, role, channel, thread_ts, created_at, message_ref, payload, abandoned_at`

// scanOutbox reads one row selected with outboxColumns.
func scanOutbox(r interface{ Scan(...any) error }) (store.OutboxEntry, error) {
	var o store.OutboxEntry
	var created, abandoned int64
	if err := r.Scan(&o.ID, &o.EventID, &o.Role, &o.Channel, &o.ThreadTS, &created, &o.MessageRef, &o.Payload, &abandoned); err != nil {
		return store.OutboxEntry{}, err
	}
	o.CreatedAt = fromTS(created)
	if abandoned != 0 {
		o.AbandonedAt = fromTS(abandoned)
	}
	return o, nil
}

// GetOutbox implements store.Outbox.
func (s *Store) GetOutbox(ctx context.Context, id string) (store.OutboxEntry, error) {
	o, err := scanOutbox(s.read.QueryRowContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.OutboxEntry{}, fmt.Errorf("get outbox %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.OutboxEntry{}, fmt.Errorf("get outbox %q: %w", id, err)
	}
	return o, nil
}

// ListUnposted implements store.Outbox.
func (s *Store) ListUnposted(ctx context.Context) ([]store.OutboxEntry, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE message_ref = '' AND abandoned_at = 0 ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list unposted outbox: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.OutboxEntry
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unposted outbox: %w", err)
	}
	return out, nil
}

// AbandonOutbox implements store.Outbox.
func (s *Store) AbandonOutbox(ctx context.Context, id string, at time.Time) (bool, error) {
	var abandoned bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE outbox SET abandoned_at = ?, payload = '' WHERE id = ? AND message_ref = '' AND abandoned_at = 0`, ts(at), id)
		if err != nil || n > 0 {
			abandoned = n > 0
			return err
		}
		var ref string // no change: the row is missing, posted, or already abandoned
		switch err := tx.QueryRowContext(ctx, `SELECT message_ref FROM outbox WHERE id = ?`, id).Scan(&ref); {
		case errors.Is(err, sql.ErrNoRows):
			return store.ErrNotFound
		case err != nil:
			return err
		case ref != "":
			return fmt.Errorf("it is posted as %s: %w", ref, store.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("abandon outbox %q: %w", id, err)
	}
	return abandoned, nil
}

// AbandonOutboxBefore implements store.Outbox.
func (s *Store) AbandonOutboxBefore(ctx context.Context, before, at time.Time) ([]string, error) {
	var ids []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `UPDATE outbox SET abandoned_at = ?, payload = ''
			WHERE message_ref = '' AND abandoned_at = 0 AND payload <> '' AND created_at < ? RETURNING id`, ts(at), ts(before))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // rows.Err below reports read failures
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("abandon old outbox rows: %w", err)
	}
	return ids, nil
}
