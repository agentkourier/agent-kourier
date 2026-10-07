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
func (s *adapter) CreateOutbox(ctx context.Context, o store.OutboxEntry) (bool, error) {
	n, err := s.exec(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, payload, trace_parent, trace_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		o.ID, o.EventID, o.Role, o.Channel, o.Thread, ts(o.CreatedAt), o.Payload, o.TraceParent, o.TraceState)
	if err != nil {
		return false, fmt.Errorf("create outbox %q: %w", o.ID, err)
	}
	return n > 0, nil
}

// RecordMessageRef implements store.Outbox.
func (s *adapter) RecordMessageRef(ctx context.Context, id, ref string) error {
	if err := s.setMessageRef(ctx, outboxRefs, id, ref); err != nil {
		return fmt.Errorf("record message ref for outbox %q: %w", id, err)
	}
	return nil
}

// outboxColumns are the columns scanOutbox reads.
const outboxColumns = `id, event_id, role, channel, thread_ts, created_at, message_ref, payload, abandoned_at, trace_parent, trace_state`

// scanOutbox reads one row selected with outboxColumns.
func scanOutbox(r interface{ Scan(...any) error }) (store.OutboxEntry, error) {
	var o store.OutboxEntry
	var created, abandoned int64
	if err := r.Scan(&o.ID, &o.EventID, &o.Role, &o.Channel, &o.Thread, &created, &o.MessageRef, &o.Payload, &abandoned, &o.TraceParent, &o.TraceState); err != nil {
		return store.OutboxEntry{}, err
	}
	o.CreatedAt = fromTS(created)
	if abandoned != 0 {
		o.AbandonedAt = fromTS(abandoned)
	}
	return o, nil
}

// GetOutbox implements store.Outbox.
func (s *adapter) GetOutbox(ctx context.Context, id string) (store.OutboxEntry, error) {
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
func (s *adapter) ListUnposted(ctx context.Context) ([]store.OutboxEntry, error) {
	//nolint:gosec // G202: the predicate is column names and literals (store.PendingOutbox); every value is an argument
	rows, err := s.read.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE `+store.PendingOutbox("").Text(Dialect{})+` ORDER BY created_at, id`)
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
func (s *adapter) AbandonOutbox(ctx context.Context, id string, at time.Time) (bool, error) {
	var abandoned bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE outbox SET abandoned_at = ?, payload = '' WHERE id = ? AND `+store.PendingOutbox("").Text(Dialect{}), ts(at), id)
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
func (s *adapter) AbandonOutboxBefore(ctx context.Context, before, at time.Time) ([]string, error) {
	var ids []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		//nolint:gosec // G202: the predicate is column names and literals (store.PendingOutbox); every value is an argument
		rows, err := tx.QueryContext(ctx, `UPDATE outbox SET abandoned_at = ?, payload = ''
			WHERE `+store.PendingOutbox("").Text(Dialect{})+` AND payload <> '' AND created_at < ? RETURNING id`, ts(at), ts(before))
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
