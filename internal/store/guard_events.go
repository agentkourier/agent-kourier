package store

import (
	"context"
	"fmt"
	"time"
)

// AppendEvent implements Events.
func (g *Guarded) AppendEvent(ctx context.Context, e Event) (inserted bool, err error) {
	// The producer's trace, not the span of this call: the queue worker continues the trace of whoever appended the event.
	stamp(ctx, &e.TraceParent, &e.TraceState)
	err = g.run(ctx, "courier.store.AppendEvent", func(ctx context.Context) error {
		if e.ID == "" || e.ReceivedAt.IsZero() {
			return fmt.Errorf("append event %q: ID and ReceivedAt are required: %w", e.ID, ErrInvalid)
		}
		var err error
		inserted, err = g.a.AppendEvent(ctx, e)
		return err
	})
	return inserted, err
}

// GetEvent implements Events.
func (g *Guarded) GetEvent(ctx context.Context, id string) (e Event, err error) {
	err = g.run(ctx, "courier.store.GetEvent", func(ctx context.Context) error {
		var err error
		e, err = g.a.GetEvent(ctx, id)
		return err
	})
	return e, err
}

// SetEventStatus implements Events.
func (g *Guarded) SetEventStatus(ctx context.Context, id string, status EventStatus, lastError string) error {
	return g.run(ctx, "courier.store.SetEventStatus", func(ctx context.Context) error {
		if !status.Valid() {
			return fmt.Errorf("set event %q status %q: %w", id, status, ErrInvalid)
		}
		return g.a.SetEventStatus(ctx, id, status, lastError)
	})
}

// ClaimEvent implements Queue.
func (g *Guarded) ClaimEvent(ctx context.Context, now time.Time, lease time.Duration) (e Event, err error) {
	err = g.run(ctx, "courier.store.ClaimEvent", func(ctx context.Context) error {
		if now.IsZero() || lease <= 0 {
			return fmt.Errorf("claim event: now must be set and lease positive: %w", ErrInvalid)
		}
		var err error
		e, err = g.a.ClaimEvent(ctx, now, lease)
		return err
	})
	return e, err
}

// CompleteEvent implements Queue.
func (g *Guarded) CompleteEvent(ctx context.Context, id string, attempt int) error {
	return g.run(ctx, "courier.store.CompleteEvent", func(ctx context.Context) error {
		if id == "" || attempt < 1 {
			return fmt.Errorf("complete event %q: ID and attempt are required: %w", id, ErrInvalid)
		}
		return g.a.CompleteEvent(ctx, id, attempt)
	})
}

// FailEvent implements Queue.
func (g *Guarded) FailEvent(ctx context.Context, id string, attempt int, retryAt time.Time, maxAttempts int, errText string) error {
	return g.run(ctx, "courier.store.FailEvent", func(ctx context.Context) error {
		if id == "" || attempt < 1 || retryAt.IsZero() || maxAttempts < 1 {
			return fmt.Errorf("fail event %q: ID and retryAt are required and attempt and maxAttempts must be at least 1: %w", id, ErrInvalid)
		}
		return g.a.FailEvent(ctx, id, attempt, retryAt, maxAttempts, errText)
	})
}
