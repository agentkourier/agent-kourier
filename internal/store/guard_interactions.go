package store

import (
	"context"
	"fmt"
	"time"
)

// CreateInteraction implements Interactions.
func (g *Guarded) CreateInteraction(ctx context.Context, i Interaction) (created bool, err error) {
	err = g.run(ctx, "agentkourier.store.CreateInteraction", func(ctx context.Context) error {
		stamp(ctx, &i.TraceParent, &i.TraceState)
		if i.ID == "" || !validKey(i.Key) || i.TaskID == "" || !i.Kind.Valid() ||
			i.CreatedAt.IsZero() || !i.Deadline.After(i.CreatedAt) {
			return fmt.Errorf("create interaction %q: ID, key, TaskID, Kind, CreatedAt and a later Deadline are required: %w", i.ID, ErrInvalid)
		}
		var err error
		created, err = g.a.CreateInteraction(ctx, i)
		return err
	})
	return created, err
}

// GetInteraction implements Interactions.
func (g *Guarded) GetInteraction(ctx context.Context, id string) (i Interaction, err error) {
	err = g.run(ctx, "agentkourier.store.GetInteraction", func(ctx context.Context) error {
		var err error
		i, err = g.a.GetInteraction(ctx, id)
		return err
	})
	return i, err
}

// PendingTextQuestion implements Interactions.
func (g *Guarded) PendingTextQuestion(ctx context.Context, key ConversationKey) (i Interaction, err error) {
	err = g.run(ctx, "agentkourier.store.PendingTextQuestion", func(ctx context.Context) error {
		if !validKey(key) {
			return fmt.Errorf("pending text question %+v: the key is required: %w", key, ErrInvalid)
		}
		var err error
		i, err = g.a.PendingTextQuestion(ctx, key)
		return err
	})
	return i, err
}

// ListDueInteractions implements Interactions.
func (g *Guarded) ListDueInteractions(ctx context.Context, now time.Time) (due []Interaction, err error) {
	err = g.run(ctx, "agentkourier.store.ListDueInteractions", func(ctx context.Context) error {
		if now.IsZero() {
			return fmt.Errorf("list due interactions: now is required: %w", ErrInvalid)
		}
		var err error
		due, err = g.a.ListDueInteractions(ctx, now)
		return err
	})
	return due, err
}

// ListDecidedInteractions implements Interactions.
func (g *Guarded) ListDecidedInteractions(ctx context.Context, since time.Time) (decided []Interaction, err error) {
	err = g.run(ctx, "agentkourier.store.ListDecidedInteractions", func(ctx context.Context) error {
		if since.IsZero() {
			return fmt.Errorf("list decided interactions: since is required: %w", ErrInvalid)
		}
		var err error
		decided, err = g.a.ListDecidedInteractions(ctx, since)
		return err
	})
	return decided, err
}

// MarkInteractionDelivered implements Interactions.
func (g *Guarded) MarkInteractionDelivered(ctx context.Context, id string, at time.Time) error {
	return g.run(ctx, "agentkourier.store.MarkInteractionDelivered", func(ctx context.Context) error {
		if id == "" || at.IsZero() {
			return fmt.Errorf("mark interaction %q delivered: an ID and a time are required: %w", id, ErrInvalid)
		}
		return g.a.MarkInteractionDelivered(ctx, id, at)
	})
}

// SubmitInteraction implements Interactions.
func (g *Guarded) SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) error {
	return g.run(ctx, "agentkourier.store.SubmitInteraction", func(ctx context.Context) error {
		if id == "" || len(answer) == 0 || decider == "" || now.IsZero() {
			return fmt.Errorf("submit interaction %q: ID, answer, decider and now are required: %w", id, ErrInvalid)
		}
		return g.a.SubmitInteraction(ctx, id, answer, decider, now)
	})
}

// ResolveInteraction implements Interactions.
func (g *Guarded) ResolveInteraction(ctx context.Context, id string, state InteractionState, now time.Time) error {
	return g.run(ctx, "agentkourier.store.ResolveInteraction", func(ctx context.Context) error {
		if id == "" || now.IsZero() || (state != InteractionTimedOut && state != InteractionCancelled) {
			return fmt.Errorf("resolve interaction %q to %q: ID, now and a timed_out or cancelled state are required: %w", id, state, ErrInvalid)
		}
		return g.a.ResolveInteraction(ctx, id, state, now)
	})
}

// SetInteractionMessageRef implements Interactions.
func (g *Guarded) SetInteractionMessageRef(ctx context.Context, id, ref string) error {
	return g.run(ctx, "agentkourier.store.SetInteractionMessageRef", func(ctx context.Context) error {
		if id == "" || ref == "" {
			return fmt.Errorf("set message ref for interaction %q: ID and ref are required: %w", id, ErrInvalid)
		}
		return g.a.SetInteractionMessageRef(ctx, id, ref)
	})
}

// ClearSettledInteractions implements Interactions.
func (g *Guarded) ClearSettledInteractions(ctx context.Context, now time.Time, grace, undeliveredGrace time.Duration) (cleared int, err error) {
	err = g.run(ctx, "agentkourier.store.ClearSettledInteractions", func(ctx context.Context) error {
		if now.IsZero() || grace < 0 || undeliveredGrace < grace {
			return fmt.Errorf("clear settled interactions: now, a grace of at least zero and an undelivered grace of at least the grace are required: %w", ErrInvalid)
		}
		var err error
		cleared, err = g.a.ClearSettledInteractions(ctx, now, grace, undeliveredGrace)
		return err
	})
	return cleared, err
}

// RecordPress implements Presses.
func (g *Guarded) RecordPress(ctx context.Context, p Press, limits PressLimits) (seq int64, err error) {
	err = g.run(ctx, "agentkourier.store.RecordPress", func(ctx context.Context) error {
		stamp(ctx, &p.TraceParent, &p.TraceState)
		if p.InteractionID == "" || p.User == "" || p.Channel == "" || p.MessageRef == "" || p.PressedAt.IsZero() || limits.Total < 1 || limits.PerUser < 1 {
			return fmt.Errorf("record press: the interaction, user, message, time and positive limits are required: %w", ErrInvalid)
		}
		var err error
		seq, err = g.a.RecordPress(ctx, p, limits)
		return err
	})
	return seq, err
}

// ListPresses implements Presses.
func (g *Guarded) ListPresses(ctx context.Context) (presses []Press, err error) {
	err = g.run(ctx, "agentkourier.store.ListPresses", func(ctx context.Context) error {
		var err error
		presses, err = g.a.ListPresses(ctx)
		return err
	})
	return presses, err
}

// PendingPresses implements Presses.
func (g *Guarded) PendingPresses(ctx context.Context, interactionID string, before time.Time) (found bool, err error) {
	err = g.run(ctx, "agentkourier.store.PendingPresses", func(ctx context.Context) error {
		if interactionID == "" || before.IsZero() {
			return fmt.Errorf("pending presses: an interaction and a time are required: %w", ErrInvalid)
		}
		var err error
		found, err = g.a.PendingPresses(ctx, interactionID, before)
		return err
	})
	return found, err
}

// DeletePress implements Presses.
func (g *Guarded) DeletePress(ctx context.Context, seq int64) error {
	return g.run(ctx, "agentkourier.store.DeletePress", func(ctx context.Context) error {
		return g.a.DeletePress(ctx, seq)
	})
}

// PurgePresses implements Presses.
func (g *Guarded) PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (purged int, err error) {
	err = g.run(ctx, "agentkourier.store.PurgePresses", func(ctx context.Context) error {
		if now.IsZero() || grace < 0 {
			return fmt.Errorf("purge presses: now and a grace of at least zero are required: %w", ErrInvalid)
		}
		var err error
		purged, err = g.a.PurgePresses(ctx, now, grace)
		return err
	})
	return purged, err
}
