package store

import (
	"context"
	"time"
)

// Reply is a thread reply waiting for its session's active task to finish
// (spec 8.2, D21).
type Reply struct {
	// ID is the turn's messageId, derived from the Slack message (spec 8.2), not the event
	// ID: an @mention arrives as two events with different IDs, and both must be one turn.
	// It makes enqueueing idempotent, and the agent sees it as the A2A messageId.
	ID  string
	Key ConversationKey
	// Sequence orders replies within a session. The store assigns it on
	// enqueue; the caller's value and clock never affect order.
	Sequence int64
	UserID   string // Slack user who wrote the reply
	// MessageTS is the Slack timestamp of the reply, for the queued reaction.
	MessageTS  string
	Text       string
	EnqueuedAt time.Time
}

// ReplyQueue persists per-session FIFO queues of replies. Delivery is
// at-least-once: the sender takes the head with NextReply, sends it, then
// marks it delivered. A delivered reply stays as a tombstone (its text is
// cleared), so a redelivered Slack event with the same ID is not queued again.
type ReplyQueue interface {
	// EnqueueReply appends r to its session's queue, idempotently by ID, and
	// reports whether it was newly enqueued; a delivered ID is a duplicate too.
	// ID, the key, UserID, MessageTS and EnqueuedAt are required (ErrInvalid
	// otherwise).
	EnqueueReply(ctx context.Context, r Reply) (enqueued bool, err error)
	// NextReply returns the oldest undelivered reply of the session, or
	// ErrNotFound if there is none.
	NextReply(ctx context.Context, key ConversationKey) (Reply, error)
	// MarkReplyDelivered records that the reply was sent at the given time and
	// discards its text. Marking again is a no-op. It returns ErrNotFound for
	// an unknown ID and ErrInvalid for an empty ID or zero time.
	MarkReplyDelivered(ctx context.Context, id string, at time.Time) error
}
