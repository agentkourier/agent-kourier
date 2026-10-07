package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// AppendAudit implements store.Audit.
func (s *adapter) AppendAudit(ctx context.Context, e store.AuditEntry) error {
	if _, err := s.exec(ctx, `INSERT INTO audit (at, kind, user_id, email, identity, binding, subject, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ts(e.At), string(e.Kind), e.User, e.Email, e.Identity, e.Binding, e.Subject, e.Outcome); err != nil {
		return fmt.Errorf("append audit %q: %w", e.Kind, err)
	}
	return nil
}

// ListAudit implements store.Audit.
func (s *adapter) ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) ([]store.AuditEntry, error) {
	from := int64(math.MinInt64)
	if !since.IsZero() {
		from = ts(since)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT sequence, at, kind, user_id, email, identity, binding, subject, outcome
		FROM audit WHERE at >= ? AND sequence > ? ORDER BY sequence LIMIT ?`, from, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []store.AuditEntry
	for rows.Next() {
		var e store.AuditEntry
		var at int64
		if err := rows.Scan(&e.Sequence, &at, &e.Kind, &e.User, &e.Email, &e.Identity, &e.Binding, &e.Subject, &e.Outcome); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		e.At = fromTS(at)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return out, nil
}

// PurgeAudit implements store.AuditRetention. The delete trigger of the audit table (migration 0020) lets a
// DELETE through only while audit_purge has a row for it, so the marker goes in, the batch out and the marker
// away again in one transaction; the single write connection keeps every other writer from seeing it.
func (s *adapter) PurgeAudit(ctx context.Context, before time.Time, limit int) (int, error) {
	var purged int64
	err := s.inTx(ctx, func(tx *sql.Tx) (err error) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_purge (cutoff) VALUES (?)`, ts(before)); err != nil {
			return err
		}
		if purged, err = execRows(ctx, tx, `DELETE FROM audit WHERE sequence IN
			(SELECT sequence FROM audit WHERE at < ? ORDER BY at, sequence LIMIT ?)`, ts(before), limit); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM audit_purge`)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("purge audit: %w", err)
	}
	return int(purged), nil
}
