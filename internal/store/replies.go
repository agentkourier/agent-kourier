package store

import (
	"context"
	"time"
)

// Reply is a thread reply waiting for its session's active task to finish
// (spec 8.2, D21).
type Reply struct {
	// TraceParent and TraceState preserve W3C ancestry for durable work; never baggage.
	TraceParent, TraceState string
	// ID is the turn's messageId, derived from the chat message (spec 8.2), not the event
	// ID: an @mention arrives as two events with different IDs, and both must be one turn.
	// It makes enqueueing idempotent, and the agent sees it as the A2A messageId.
	ID  string
	Key ConversationKey
	// Sequence orders replies within a session. The store assigns it on
	// enqueue; the caller's value and clock never affect order. It need not be comparable between sessions.
	Sequence int64
	UserID   string // the platform's ID of the user who wrote the reply
	// MessageRef is the platform's ID of the reply message, for the queued reaction.
	MessageRef string
	// Acknowledgment is the configured bare reaction name to remove when this reply's processing ends.
	// It is persisted with the reply so a restart or configuration reload keeps the reaction it added.
	Acknowledgment string
	// InteractionID pins a text reply to its first question across recovery.
	InteractionID string
	Text          string
	EnqueuedAt    time.Time
}

// MaxReplyIDs is the most IDs that one call that takes many reply IDs (HasReplies, AcknowledgmentLister's
// SettleAcknowledgmentReplies) may name (ErrInvalid above it). It bounds the size of one call, and so of the statement or
// the chunks of statements that an adapter runs for it, not the answer; a caller with more makes several calls.
const MaxReplyIDs = 10000

// ReplyQueue persists per-session FIFO queues of replies. Delivery is
// at-least-once: the sender takes the head with NextReply, sends it, then
// marks it delivered. A delivered reply stays as a tombstone (its text is
// cleared), so a redelivered chat event with the same ID is not queued again.
//
// A reader that walks a session's queue with NextReplyAfter must never miss a reply that was
// enqueued while it walked: a reply becomes visible only after every reply with a lower Sequence in
// its session is. A store whose Sequence is assigned at insert and shown at commit must serialise
// the enqueues of a session to keep this.
type ReplyQueue interface {
	// EnqueueReply appends r to its session's queue, idempotently by ID, and
	// reports whether it was newly enqueued; a delivered ID is a duplicate too.
	// ID, the key, UserID, MessageRef and EnqueuedAt are required (ErrInvalid
	// otherwise).
	EnqueueReply(ctx context.Context, r Reply) (enqueued bool, err error)
	// HasReply reports whether a reply with this ID was ever enqueued, queued or delivered (a delivered one stays as
	// a tombstone). It is how a caller tells a turn that was durably taken from one that never was. ErrInvalid for an
	// empty ID.
	HasReply(ctx context.Context, id string) (bool, error)
	// HasReplies is HasReply for many IDs in one round trip: it returns the set of the IDs that were ever enqueued,
	// queued or delivered, as a map whose every entry is true. IDs that were never enqueued are absent, a repeated ID
	// counts once, and no IDs give an empty map (never nil) without a query. ErrInvalid for an empty ID among them or
	// more than MaxReplyIDs.
	HasReplies(ctx context.Context, ids []string) (map[string]bool, error)
	// NextReply returns the oldest undelivered reply of the session, or
	// ErrNotFound if there is none.
	NextReply(ctx context.Context, key ConversationKey) (Reply, error)
	// NextReplyAfter reads the oldest undelivered reply whose sequence exceeds after.
	// Skipped rows remain queued; normal turns still use NextReply.
	NextReplyAfter(ctx context.Context, key ConversationKey, after int64) (Reply, error)
	// PreviousReply returns the session's row with the greatest Sequence below the given one, whatever its state: a
	// delivered row (a tombstone, its text cleared), a dropped one and one pinned to an interaction all count, and
	// another session's rows never do. It is how a turn finds the message that ended the window of the turn before it
	// (spec 8.2, D38), and it is the same on every attempt, after a restart and after a failover, because it reads
	// stored rows. ErrNotFound if the session has no row below it, as for its first turn; ErrInvalid for an invalid
	// key or a sequence below 1.
	PreviousReply(ctx context.Context, key ConversationKey, sequence int64) (Reply, error)
	// MarkReplyDelivered records that the reply was sent at the given time and
	// discards its text. Marking again is a no-op. It returns ErrNotFound for
	// an unknown ID and ErrInvalid for an empty ID or zero time.
	MarkReplyDelivered(ctx context.Context, id string, at time.Time) error
	// AbandonStrandedReplies is the time backstop for a reply that is never drained: it drops every
	// undelivered reply enqueued at least age before now, in the way a turn is dropped when its Binding
	// has left (the text is cleared and the row is marked delivered at now, so it stays a tombstone and
	// is never sent), and returns how many. A reply whose session has an active task is left alone
	// however old: it waits behind that task by design. age must be positive and now not zero
	// (ErrInvalid otherwise).
	AbandonStrandedReplies(ctx context.Context, now time.Time, age time.Duration) (int, error)
}

// ReplyInteractionClaimer pins a queued answer to one question. It is not part of ReplyQueue, which the session
// manager takes: only the interactions package claims a reply.
type ReplyInteractionClaimer interface {
	// ClaimReplyInteraction pins the queued reply id to the question interaction. A retry with the same pair changes
	// nothing and succeeds; a reply already pinned to another question is left as it is, and the call returns
	// ErrNotFound, as it does for a reply that is not queued. An empty id or interaction is ErrInvalid.
	ClaimReplyInteraction(ctx context.Context, id, interaction string) error
}

// AcknowledgmentLister is what retention reads to retry an acknowledgment's removal. It lists delivered tombstones
// whose acknowledgment metadata still exists and whose originating task is no longer active (AcknowledgmentHeld), so
// the broker can enqueue durable removals without racing a task that is still processing the message.
type AcknowledgmentLister interface {
	ListAcknowledgmentReplies(ctx context.Context) ([]Reply, error)
	// SettleAcknowledgmentReplies records that the removals of the acknowledgments of the delivered replies ids are
	// durably queued: it clears their acknowledgment metadata, so neither lister returns them again. Without it every
	// tombstone would be listed, and its removal queued again, at each retention tick until its session is deleted.
	// An undelivered reply, whose add may still be needed, and an unknown ID are left alone, and a repeat changes
	// nothing. ErrInvalid for an empty ID among them or more than MaxReplyIDs.
	SettleAcknowledgmentReplies(ctx context.Context, ids []string) error
}

// PendingAcknowledgmentLister is what retention reads to retry an acknowledgment's add. It lists replies that carry
// acknowledgment metadata while their add may still be needed: an undelivered reply, or a delivered reply whose
// originating task is still active (AcknowledgmentHeld). A paired removal in the outbox makes a retry harmless after
// the task has ended.
type PendingAcknowledgmentLister interface {
	ListPendingAcknowledgmentReplies(ctx context.Context) ([]Reply, error)
}
