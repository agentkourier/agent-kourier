package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// AppendAudit implements store.Audit. Appends run one at a time: the transaction takes the log's
// advisory lock before it inserts and holds it until it commits, so an entry is shown only after every
// entry with a lower Sequence is, which is what a reader that pages with ListAudit relies on.
func (s *Store) AppendAudit(ctx context.Context, e store.AuditEntry) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.AppendAudit")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	e = e.Redacted()
	if !e.Kind.Valid() || e.At.IsZero() || e.Subject == "" || e.Outcome == "" || len(e.Outcome) > store.MaxAuditOutcome {
		return fmt.Errorf("append audit %q: valid Kind, At, Subject and an Outcome of 1 to %d bytes are required: %w",
			e.Kind, store.MaxAuditOutcome, store.ErrInvalid)
	}
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
func (s *Store) ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) (entries []store.AuditEntry, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListAudit")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("list audit: limit %d outside 1 to 1000: %w", limit, store.ErrInvalid)
	}
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
