package store

import (
	"context"
	"fmt"
	"time"
)

// AppendAudit implements Audit. The entry is stored as Redacted returns it, and the outcome's length is measured on
// that text.
func (g *Guarded) AppendAudit(ctx context.Context, e AuditEntry) error {
	return g.run(ctx, "courier.store.AppendAudit", func(ctx context.Context) error {
		e = e.Redacted()
		if !e.Kind.Valid() || e.At.IsZero() || e.Subject == "" || e.Outcome == "" || len(e.Outcome) > MaxAuditOutcome {
			return fmt.Errorf("append audit %q: valid Kind, At, Subject and an Outcome of 1 to %d bytes are required: %w",
				e.Kind, MaxAuditOutcome, ErrInvalid)
		}
		return g.a.AppendAudit(ctx, e)
	})
}

// ListAudit implements Audit.
func (g *Guarded) ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) (entries []AuditEntry, err error) {
	err = g.run(ctx, "courier.store.ListAudit", func(ctx context.Context) error {
		if limit < 1 || limit > 1000 {
			return fmt.Errorf("list audit: limit %d outside 1 to 1000: %w", limit, ErrInvalid)
		}
		var err error
		entries, err = g.a.ListAudit(ctx, since, afterSeq, limit)
		return err
	})
	return entries, err
}

// PurgeAudit implements AuditRetention.
func (g *Guarded) PurgeAudit(ctx context.Context, before time.Time, limit int) (purged int, err error) {
	err = g.run(ctx, "courier.store.PurgeAudit", func(ctx context.Context) error {
		if before.IsZero() || !validLimit(limit) {
			return fmt.Errorf("purge audit: a cutoff and a limit of 1 to %d are required: %w", MaxLimit, ErrInvalid)
		}
		var err error
		purged, err = g.a.PurgeAudit(ctx, before, limit)
		return err
	})
	return purged, err
}

// RecordVote implements Feedback.
func (g *Guarded) RecordVote(ctx context.Context, v FeedbackVote) (previous Verdict, err error) {
	err = g.run(ctx, "courier.store.RecordVote", func(ctx context.Context) error {
		if !validKey(v.Key) || v.Turn == "" || v.User == "" || v.Binding == "" || v.Agent == "" || !v.Kind.Valid() || !v.Verdict.Valid() || !validTime(v.At) {
			return fmt.Errorf("record vote: the session, turn, user, Binding, agent, a kind, a verdict and a time after the Unix epoch are required: %w", ErrInvalid)
		}
		var err error
		previous, err = g.a.RecordVote(ctx, v)
		return err
	})
	return previous, err
}

// ListVotes implements Feedback.
func (g *Guarded) ListVotes(ctx context.Context, key ConversationKey) (votes []FeedbackVote, err error) {
	err = g.run(ctx, "courier.store.ListVotes", func(ctx context.Context) error {
		if !validKey(key) {
			return fmt.Errorf("list votes %+v: the key is incomplete: %w", key, ErrInvalid)
		}
		var err error
		votes, err = g.a.ListVotes(ctx, key)
		return err
	})
	return votes, err
}
