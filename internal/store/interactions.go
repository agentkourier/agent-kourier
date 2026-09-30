package store

import (
	"context"
	"time"
)

// InteractionKind is the kind of pause an agent is waiting on (spec 8.4).
type InteractionKind string

// Interaction kinds.
const (
	InteractionAskUser      InteractionKind = "ask_user"
	InteractionToolApproval InteractionKind = "tool_approval"
	InteractionNested       InteractionKind = "nested"
)

// Valid reports whether k is one of the defined kinds.
func (k InteractionKind) Valid() bool {
	switch k {
	case InteractionAskUser, InteractionToolApproval, InteractionNested:
		return true
	}
	return false
}

// InteractionState is where an interaction is in its life.
type InteractionState string

// Interaction states. Only a pending interaction can change state.
const (
	InteractionPending   InteractionState = "pending"
	InteractionAnswered  InteractionState = "answered"
	InteractionTimedOut  InteractionState = "timed_out"
	InteractionCancelled InteractionState = "cancelled"
)

// Valid reports whether s is one of the defined states.
func (s InteractionState) Valid() bool {
	switch s {
	case InteractionPending, InteractionAnswered, InteractionTimedOut, InteractionCancelled:
		return true
	}
	return false
}

// Interaction is one pause waiting on a person (spec 9, table interactions).
// Request and Answer are opaque to the store: the dialect owns their shape.
type Interaction struct {
	ID     string
	Key    ConversationKey
	TaskID string
	Kind   InteractionKind
	// State is always pending on creation; CreateInteraction ignores it.
	State   InteractionState
	Request []byte
	// MessageRef is the rendered Slack message. It is empty until
	// SetInteractionMessageRef records it; CreateInteraction ignores it.
	MessageRef string
	CreatedAt  time.Time
	Deadline   time.Time

	// The fields below are set when the interaction is decided, and are
	// ignored by CreateInteraction.
	Answer    []byte
	Decider   string // Slack user ID
	DecidedAt time.Time
}

// Interactions persists interactions. For CreateInteraction, ID, the three key
// parts, TaskID, a valid Kind, CreatedAt and a Deadline after CreatedAt are
// required. A missing or illegal required value returns ErrInvalid.
type Interactions interface {
	// CreateInteraction stores i as pending, idempotently by ID, and reports
	// whether it was newly inserted.
	CreateInteraction(ctx context.Context, i Interaction) (created bool, err error)
	// GetInteraction returns the interaction with the given ID, or ErrNotFound.
	GetInteraction(ctx context.Context, id string) (Interaction, error)
	// ListDueInteractions returns pending interactions whose deadline is at or
	// before now, oldest deadline first, for the timeout sweep.
	ListDueInteractions(ctx context.Context, now time.Time) ([]Interaction, error)
	// SubmitInteraction records the first complete answer: an atomic
	// transition from pending to answered, valid while now is before the
	// deadline. answer, decider and now are required. Errors: ErrNotFound if
	// the interaction does not exist; ErrExpired if it is past its deadline
	// (pending or already timed out); ErrConflict, naming the decider when
	// there is one, if it was already answered or cancelled.
	SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) error
	// ResolveInteraction moves a pending interaction to state, which must be
	// InteractionTimedOut or InteractionCancelled, and records now as the
	// decision time. Timing out also requires the deadline to be at or before
	// now; cancelling does not. It returns ErrNotFound if the interaction does
	// not exist and ErrConflict if it is no longer pending or not yet due.
	ResolveInteraction(ctx context.Context, id string, state InteractionState, now time.Time) error
	// SetInteractionMessageRef records the rendered message, in any state
	// (a fast answer can land first). It is a compare-and-set: the same ref
	// again is a no-op, a different ref over an existing one returns
	// ErrConflict, a missing interaction ErrNotFound, and an empty id or ref
	// ErrInvalid.
	SetInteractionMessageRef(ctx context.Context, id, ref string) error
}
