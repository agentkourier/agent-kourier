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
	// MessageRef is the rendered Slack message, if known at creation.
	MessageRef string
	CreatedAt  time.Time
	Deadline   time.Time

	// The fields below are set when the interaction is decided, and are
	// ignored by CreateInteraction.
	Answer    []byte
	Decider   string // Slack user ID
	DecidedAt time.Time
}

// Interactions persists interactions. ID, the three key parts, TaskID, a valid
// Kind, CreatedAt and a Deadline after CreatedAt are required; anything else
// returns ErrInvalid.
type Interactions interface {
	// CreateInteraction stores i as pending, idempotently by ID, and reports
	// whether it was newly inserted.
	CreateInteraction(ctx context.Context, i Interaction) (created bool, err error)
	// GetInteraction returns the interaction with the given ID, or ErrNotFound.
	GetInteraction(ctx context.Context, id string) (Interaction, error)
	// ListDueInteractions returns pending interactions whose deadline is at or
	// before now, oldest deadline first, for the timeout sweep.
	ListDueInteractions(ctx context.Context, now time.Time) ([]Interaction, error)
}
