package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// EnqueueReply implements ReplyQueue.
func (g *Guarded) EnqueueReply(ctx context.Context, r Reply) (enqueued bool, err error) {
	err = g.run(ctx, "agentkourier.store.EnqueueReply", func(ctx context.Context) error {
		stamp(ctx, &r.TraceParent, &r.TraceState)
		if r.ID == "" || !validKey(r.Key) || r.UserID == "" || r.MessageRef == "" || r.EnqueuedAt.IsZero() {
			return fmt.Errorf("enqueue reply %q: ID, key, UserID, MessageRef and EnqueuedAt are required: %w", r.ID, ErrInvalid)
		}
		var err error
		enqueued, err = g.a.EnqueueReply(ctx, r)
		return err
	})
	return enqueued, err
}

// HasReply implements ReplyQueue.
func (g *Guarded) HasReply(ctx context.Context, id string) (found bool, err error) {
	err = g.run(ctx, "agentkourier.store.HasReply", func(ctx context.Context) error {
		if id == "" {
			return fmt.Errorf("has reply: an ID is required: %w", ErrInvalid)
		}
		var err error
		found, err = g.a.HasReply(ctx, id)
		return err
	})
	return found, err
}

// HasReplies implements ReplyQueue.
func (g *Guarded) HasReplies(ctx context.Context, ids []string) (found map[string]bool, err error) {
	err = g.run(ctx, "agentkourier.store.HasReplies", func(ctx context.Context) error {
		if len(ids) > MaxReplyIDs || slices.Contains(ids, "") {
			return fmt.Errorf("has replies: %d IDs, none empty and at most %d, are required: %w", len(ids), MaxReplyIDs, ErrInvalid)
		}
		var err error
		found, err = g.a.HasReplies(ctx, ids)
		return err
	})
	return found, err
}

// NextReply implements ReplyQueue.
func (g *Guarded) NextReply(ctx context.Context, key ConversationKey) (r Reply, err error) {
	err = g.run(ctx, "agentkourier.store.NextReply", func(ctx context.Context) error {
		if !validKey(key) {
			return fmt.Errorf("next reply %+v: key is required: %w", key, ErrInvalid)
		}
		var err error
		r, err = g.a.NextReply(ctx, key)
		return err
	})
	return r, err
}

// NextReplyAfter implements ReplyQueue.
func (g *Guarded) NextReplyAfter(ctx context.Context, key ConversationKey, after int64) (r Reply, err error) {
	err = g.run(ctx, "agentkourier.store.NextReplyAfter", func(ctx context.Context) error {
		if !validKey(key) || after < 0 {
			return fmt.Errorf("next reply %+v: key is required: %w", key, ErrInvalid)
		}
		var err error
		r, err = g.a.NextReplyAfter(ctx, key, after)
		return err
	})
	return r, err
}

// PreviousReply implements ReplyQueue.
func (g *Guarded) PreviousReply(ctx context.Context, key ConversationKey, sequence int64) (r Reply, err error) {
	err = g.run(ctx, "agentkourier.store.PreviousReply", func(ctx context.Context) error {
		if !validKey(key) || sequence < 1 {
			return fmt.Errorf("previous reply %+v below %d: a key and a sequence from 1 are required: %w", key, sequence, ErrInvalid)
		}
		var err error
		r, err = g.a.PreviousReply(ctx, key, sequence)
		return err
	})
	return r, err
}

// MarkReplyDelivered implements ReplyQueue.
func (g *Guarded) MarkReplyDelivered(ctx context.Context, id string, at time.Time) error {
	return g.run(ctx, "agentkourier.store.MarkReplyDelivered", func(ctx context.Context) error {
		if id == "" || at.IsZero() {
			return fmt.Errorf("mark reply %q delivered: ID and time are required: %w", id, ErrInvalid)
		}
		return g.a.MarkReplyDelivered(ctx, id, at)
	})
}

// AbandonStrandedReplies implements ReplyQueue.
func (g *Guarded) AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (abandoned int, err error) {
	err = g.run(ctx, "agentkourier.store.AbandonStrandedReplies", func(ctx context.Context) error {
		if now.IsZero() || age <= 0 {
			return fmt.Errorf("abandon stranded replies: a time and a positive age are required: %w", ErrInvalid)
		}
		var err error
		abandoned, err = g.a.AbandonStrandedReplies(ctx, now, age)
		return err
	})
	return abandoned, err
}

// ListAcknowledgmentReplies implements AcknowledgmentLister. It is used by
// retention to retry removals whose replies were already delivered.
func (g *Guarded) ListAcknowledgmentReplies(ctx context.Context, now time.Time, age time.Duration) (replies []Reply, err error) {
	err = g.run(ctx, "agentkourier.store.ListAcknowledgmentReplies", func(ctx context.Context) error {
		if now.IsZero() || age <= 0 {
			return fmt.Errorf("list acknowledgment replies: a time and a positive age are required: %w", ErrInvalid)
		}
		lister, ok := g.a.(AcknowledgmentLister)
		if !ok {
			return fmt.Errorf("list acknowledgment replies: adapter does not support retention metadata: %w", ErrInvalid)
		}
		var err error
		replies, err = lister.ListAcknowledgmentReplies(ctx, now, age)
		return err
	})
	return replies, err
}

// ListPendingAcknowledgmentReplies implements PendingAcknowledgmentLister. It
// is used by retention to retry acknowledgment additions for replies whose task
// is still active or whose queue row has not been delivered.
func (g *Guarded) ListPendingAcknowledgmentReplies(ctx context.Context) (replies []Reply, err error) {
	err = g.run(ctx, "agentkourier.store.ListPendingAcknowledgmentReplies", func(ctx context.Context) error {
		lister, ok := g.a.(PendingAcknowledgmentLister)
		if !ok {
			return fmt.Errorf("list pending acknowledgment replies: adapter does not support retention metadata: %w", ErrInvalid)
		}
		var err error
		replies, err = lister.ListPendingAcknowledgmentReplies(ctx)
		return err
	})
	return replies, err
}
