package store

import (
	"context"
	"time"
)

// Press is a button press recorded before it is processed: what Slack sent, kept so that a crash
// between the acknowledgement and the work loses nothing (spec 8.4).
type Press struct {
	// TraceParent and TraceState preserve W3C ancestry for durable work; never baggage.
	TraceParent, TraceState string
	// Seq orders presses by when they were recorded; the store assigns it, and never gives the Seq of
	// a deleted press to another, so that DeletePress cannot remove a later press by mistake.
	Seq int64
	// InteractionID is the pressed button's value, which names the interaction it answers.
	InteractionID string
	// User is who pressed, as Slack says.
	User string
	// Channel and MessageRef locate the message the button is on.
	Channel, MessageRef string
	// Inputs is the press's input state as the caller encoded it; the store does not read it. It
	// holds what the person typed, so it is deleted as soon as the press is processed.
	Inputs    []byte
	PressedAt time.Time
}

// PressLimits bounds the presses waiting to be processed, so that one person or a flood cannot fill
// the store.
type PressLimits struct {
	Total, PerUser int
}

// Presses persists the presses waiting to be processed. A press that has been processed is deleted:
// the table holds only work to do.
type Presses interface {
	// RecordPress stores p and returns its Seq. InteractionID, User, Channel, MessageRef and
	// PressedAt are required (ErrInvalid), and both limits are positive. It returns ErrLimit if
	// the store already holds limits.Total presses, or limits.PerUser for p.User.
	RecordPress(ctx context.Context, p Press, limits PressLimits) (int64, error)
	// ListPresses returns the presses waiting, oldest Seq first.
	ListPresses(ctx context.Context) ([]Press, error)
	// PendingPresses reports whether a press of the interaction is waiting that was made at or
	// before before: one a decision at that time would have had to take into account. The timeout
	// sweep asks it before it times an interaction out. InteractionID and before are required.
	//
	// An answer typed as a reply in the thread is a press not yet recorded, so it counts too: a reply
	// that waits in the reply queue pinned to the interaction, and enqueued at or before before; or,
	// for an interaction that is pending, takes text replies and has its prompt posted, one not pinned
	// to any question that was enqueued from the interaction's creation to before.
	PendingPresses(ctx context.Context, interactionID string, before time.Time) (bool, error)
	// DeletePress removes the press with seq, once it is processed. A press that is already gone is
	// not an error: a crash may repeat the call.
	DeletePress(ctx context.Context, seq int64) error
	// PurgePresses deletes the presses that have waited past their interaction's deadline plus
	// grace (an interaction that is not in the store counts from the press's own time), and returns
	// how many.
	PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (int, error)
}
