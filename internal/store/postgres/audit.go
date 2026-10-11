package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// AppendAudit implements store.Audit. Appends run one at a time: the transaction takes the log's
// advisory lock before it inserts and holds it until it commits, so an entry is shown only after every
// entry with a lower Sequence is, which is what a reader that pages with ListAudit relies on.
func (s *adapter) AppendAudit(ctx context.Context, e store.AuditEntry) error {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, lockAudit, ""); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO audit (at, kind, user_id, email, identity, binding, subject, outcome)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, e.At, string(e.Kind), e.User, e.Email, e.Identity, e.Binding, e.Subject, e.Outcome)
		return err
	})
	if err != nil {
		return fmt.Errorf("append audit %q: %w", e.Kind, err)
	}
	return nil
}

// ListAudit implements store.Audit.
func (s *adapter) ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) ([]store.AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT sequence, at, kind, user_id, email, identity, binding, subject, outcome
		FROM audit WHERE ($1::timestamptz IS NULL OR at >= $1) AND sequence > $2 ORDER BY sequence LIMIT $3`, nullable(since), afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	var out []store.AuditEntry
	for rows.Next() {
		var e store.AuditEntry
		var at time.Time
		if err := rows.Scan(&e.Sequence, &at, &e.Kind, &e.User, &e.Email, &e.Identity, &e.Binding, &e.Subject, &e.Outcome); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		e.At = utc(at)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return out, nil
}

// PurgeAudit implements store.AuditRetention through agentkourier_audit_purge (migration 0023 added it, and 0024 dropped
// the courier_audit_purge of 0006 and 0020 that it replaced), which sets the setting that
// lets the audit table's delete trigger through, local to its transaction. It is a write like any other: the epoch
// is checked first, so only the leader purges.
func (s *adapter) PurgeAudit(ctx context.Context, before time.Time, limit int) (int, error) {
	var n int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT agentkourier_audit_purge($1, $2::integer)`, before, limit).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("purge audit: %w", err)
	}
	return int(n), nil
}
