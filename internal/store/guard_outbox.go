package store

import (
	"context"
	"fmt"
	"time"
)

// CreateOutbox implements Outbox.
func (g *Guarded) CreateOutbox(ctx context.Context, o OutboxEntry) (created bool, err error) {
	err = g.run(ctx, "courier.store.CreateOutbox", func(ctx context.Context) error {
		stamp(ctx, &o.TraceParent, &o.TraceState)
		if o.ID == "" || o.EventID == "" || o.Channel == "" || o.CreatedAt.IsZero() || !o.Role.Valid() {
			return fmt.Errorf("create outbox %q: ID, EventID, a valid Role, Channel and CreatedAt are required: %w", o.ID, ErrInvalid)
		}
		var err error
		created, err = g.a.CreateOutbox(ctx, o)
		return err
	})
	return created, err
}

// RecordMessageRef implements Outbox.
func (g *Guarded) RecordMessageRef(ctx context.Context, id, ref string) error {
	return g.run(ctx, "courier.store.RecordMessageRef", func(ctx context.Context) error {
		if id == "" || ref == "" {
			return fmt.Errorf("record message ref for outbox %q: ID and ref are required: %w", id, ErrInvalid)
		}
		return g.a.RecordMessageRef(ctx, id, ref)
	})
}

// GetOutbox implements Outbox.
func (g *Guarded) GetOutbox(ctx context.Context, id string) (o OutboxEntry, err error) {
	err = g.run(ctx, "courier.store.GetOutbox", func(ctx context.Context) error {
		var err error
		o, err = g.a.GetOutbox(ctx, id)
		return err
	})
	return o, err
}

// ListUnposted implements Outbox.
func (g *Guarded) ListUnposted(ctx context.Context) (entries []OutboxEntry, err error) {
	err = g.run(ctx, "courier.store.ListUnposted", func(ctx context.Context) error {
		var err error
		entries, err = g.a.ListUnposted(ctx)
		return err
	})
	return entries, err
}

// AbandonOutbox implements Outbox.
func (g *Guarded) AbandonOutbox(ctx context.Context, id string, at time.Time) (abandoned bool, err error) {
	err = g.run(ctx, "courier.store.AbandonOutbox", func(ctx context.Context) error {
		if !validTime(at) {
			return fmt.Errorf("abandon outbox %q at %v: the time must be after the Unix epoch: %w", id, at, ErrInvalid)
		}
		var err error
		abandoned, err = g.a.AbandonOutbox(ctx, id, at)
		return err
	})
	return abandoned, err
}

// AbandonOutboxBefore implements Outbox.
func (g *Guarded) AbandonOutboxBefore(ctx context.Context, before, at time.Time) (ids []string, err error) {
	err = g.run(ctx, "courier.store.AbandonOutboxBefore", func(ctx context.Context) error {
		if !validTime(before) || !validTime(at) {
			return fmt.Errorf("abandon old outbox rows (before %v, at %v): both times must be after the Unix epoch: %w", before, at, ErrInvalid)
		}
		var err error
		ids, err = g.a.AbandonOutboxBefore(ctx, before, at)
		return err
	})
	return ids, err
}
