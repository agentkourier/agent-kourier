package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// refTable names a table with a message_ref column. It is spliced into SQL, so only the constants
// below may be used; caller input needs an explicit conversion to reach it.
type refTable string

const (
	outboxRefs      refTable = "outbox"
	interactionRefs refTable = "interactions"
)

// setMessageRef is a compare-and-set of message_ref on table. The UPDATE and the read that tells a
// missing row from a conflict share one transaction, so the answer reflects the state the UPDATE saw.
// An outbox row's payload is cleared in the same UPDATE: once the message exists, its text lives in
// the chat platform. Of concurrent calls with different refs the row lock lets one UPDATE match.
func (s *adapter) setMessageRef(ctx context.Context, table refTable, id, ref string) error {
	set := "message_ref = $1"
	if table == outboxRefs {
		set += ", payload = ''"
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE `+string(table)+` SET `+set+` WHERE id = $2 AND message_ref IN ('', $1)`, ref, id)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT message_ref FROM `+string(table)+` WHERE id = $1`, id).Scan(&existing)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return store.ErrNotFound
		case err != nil:
			return err
		case existing == ref:
			return nil
		}
		return fmt.Errorf("already recorded as a different message: %w", store.ErrConflict)
	})
}

// CreateOutbox implements store.Outbox.
func (s *adapter) CreateOutbox(ctx context.Context, o store.OutboxEntry) (bool, error) {
	n, err := s.exec(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, payload, trace_parent, trace_state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`,
		o.ID, o.EventID, string(o.Role), o.Channel, o.Thread, o.CreatedAt, o.Payload, o.TraceParent, o.TraceState)
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
func scanOutbox(r pgx.Row) (store.OutboxEntry, error) {
	var o store.OutboxEntry
	var created time.Time
	var abandoned *time.Time
	if err := r.Scan(&o.ID, &o.EventID, &o.Role, &o.Channel, &o.Thread, &created, &o.MessageRef, &o.Payload, &abandoned, &o.TraceParent, &o.TraceState); err != nil {
		return store.OutboxEntry{}, err
	}
	o.CreatedAt = utc(created)
	o.AbandonedAt = fromNull(abandoned)
	return o, nil
}

// GetOutbox implements store.Outbox.
func (s *adapter) GetOutbox(ctx context.Context, id string) (store.OutboxEntry, error) {
	o, err := scanOutbox(s.pool.QueryRow(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OutboxEntry{}, fmt.Errorf("get outbox %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.OutboxEntry{}, fmt.Errorf("get outbox %q: %w", id, err)
	}
	return o, nil
}

// ListUnposted implements store.Outbox.
func (s *adapter) ListUnposted(ctx context.Context) ([]store.OutboxEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE `+store.PendingOutbox("").Text(Dialect{})+` ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list unposted outbox: %w", err)
	}
	defer rows.Close()
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

// AbandonOutbox implements store.Outbox. The times are checked as SQLite's must be (the epoch is its
// "never"), so that the stores accept the same calls.
func (s *adapter) AbandonOutbox(ctx context.Context, id string, at time.Time) (bool, error) {
	var abandoned bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE outbox SET abandoned_at = $1, payload = '' WHERE id = $2 AND `+store.PendingOutbox("").Text(Dialect{}), at, id)
		if err != nil || tag.RowsAffected() > 0 {
			abandoned = tag.RowsAffected() > 0
			return err
		}
		var ref string // no change: the row is missing, posted, or already abandoned
		switch err := tx.QueryRow(ctx, `SELECT message_ref FROM outbox WHERE id = $1`, id).Scan(&ref); {
		case errors.Is(err, pgx.ErrNoRows):
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

// AbandonOutboxBefore implements store.Outbox. Of concurrent purges each row is taken by one: a purge
// that meets a row another is changing waits for it, and then finds it no longer qualifies.
func (s *adapter) AbandonOutboxBefore(ctx context.Context, before, at time.Time) ([]string, error) {
	var ids []string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE outbox SET abandoned_at = $1, payload = ''
			WHERE `+store.PendingOutbox("").Text(Dialect{})+` AND payload <> '' AND created_at < $2 RETURNING id`, at, before)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("abandon old outbox rows: %w", err)
	}
	return ids, nil
}
