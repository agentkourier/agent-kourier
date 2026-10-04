package sqlite

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// AppendAudit implements store.Audit.
func (s *Store) AppendAudit(ctx context.Context, e store.AuditEntry) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.AppendAudit")
	defer func() { finishStore(op, telemetryErr) }()
	e = e.Redacted()
	if !e.Kind.Valid() || e.At.IsZero() || e.Subject == "" || e.Outcome == "" || len(e.Outcome) > store.MaxAuditOutcome {
		return fmt.Errorf("append audit %q: valid Kind, At, Subject and an Outcome of 1 to %d bytes are required: %w",
			e.Kind, store.MaxAuditOutcome, store.ErrInvalid)
	}
	if _, err := s.exec(ctx, `INSERT INTO audit (at, kind, user_id, email, identity, binding, subject, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ts(e.At), string(e.Kind), e.User, e.Email, e.Identity, e.Binding, e.Subject, e.Outcome); err != nil {
		return fmt.Errorf("append audit %q: %w", e.Kind, err)
	}
	return nil
}

// ListAudit implements store.Audit.
func (s *Store) ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) (telemetryResult0 []store.AuditEntry, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListAudit")
	defer func() { finishStore(op, telemetryErr) }()
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("list audit: limit %d outside 1 to 1000: %w", limit, store.ErrInvalid)
	}
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
