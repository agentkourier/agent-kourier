package store

import (
	"context"
	"errors"
	"fmt"
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
	// Connection is the ChatConnection ("namespace/name") the press came in on: the press cap is counted per connection,
	// and a channel ID names a channel only within one app. Empty is a press recorded before the broker recorded
	// connections, which belongs to the broker's own connection until AttributeConnection says so.
	Connection string
	// Channel and MessageRef locate the message the button is on.
	Channel, MessageRef string
	// Inputs is the press's input state as the caller encoded it; the store does not read it. It
	// holds what the person typed, so it is deleted as soon as the press is processed.
	Inputs    []byte
	PressedAt time.Time
	// Delivery is the chat platform's key of the press: a delivery of the same press again (a retry of a request whose
	// answer was lost) carries the same key, and another press, the same person's second click included, another. The
	// store does not read it but to record it once: a second RecordPress of a key of the connection returns ErrDuplicate,
	// as long as the key is kept (PurgePresses). Empty is a press with no key, which is recorded each time it comes.
	// ListPresses does not return it.
	Delivery string
}

// PressLimits bounds the presses waiting to be processed, so that one person or a flood cannot fill
// the store. Both bounds are counted per connection (Press.Connection), so a flood on one app cannot refuse
// the presses of another; the bound for the whole process is the limit times the connections it serves.
type PressLimits struct {
	Total, PerUser int
}

// Presses persists the presses waiting to be processed. A press that has been processed is deleted:
// the table holds only work to do.
type Presses interface {
	// RecordPress stores p and returns its Seq. InteractionID, User, Channel, MessageRef and
	// PressedAt are required (ErrInvalid), and both limits are positive. It returns ErrDuplicate, and records
	// nothing, if p.Delivery is not empty and a press of p.Connection with that key was recorded before, whether or
	// not it has been processed since; with it, the Seq of that press while it waits, and 0 once it is processed or
	// purged. That is asked before the limits, so a press that is sent again is never refused as one too many. It
	// returns ErrLimit if the store already holds limits.Total presses of p.Connection, or limits.PerUser for p.User
	// on it. RecordOnce is how intake calls it.
	RecordPress(ctx context.Context, p Press, limits PressLimits) (int64, error)
	// GetPress returns the press with seq while it waits, as it was recorded, and ErrNotFound once it is processed
	// or purged. seq is positive (ErrInvalid).
	GetPress(ctx context.Context, seq int64) (Press, error)
	// ListPresses returns the presses waiting, oldest Seq first.
	ListPresses(ctx context.Context) ([]Press, error)
	// PendingPresses reports whether a press of the interaction is waiting that was made at or
	// before before: one a decision at that time would have had to take into account. The timeout
	// sweep asks it before it times an interaction out. InteractionID and before are required.
	//
	// An answer typed as a reply in the thread is a press not yet recorded, so it counts too: a reply
	// that waits in the reply queue pinned to the interaction, and enqueued at or before before; or,
	// for an interaction that is pending, takes text replies and has its prompt posted, one not pinned
	// to any question that was enqueued from the interaction's creation to before, and that a person wrote: a turn filed
	// under SystemUser is never an answer.
	PendingPresses(ctx context.Context, interactionID string, before time.Time) (bool, error)
	// DeletePress removes the press with seq, once it is processed. A press that is already gone is
	// not an error: a crash may repeat the call.
	DeletePress(ctx context.Context, seq int64) error
	// PurgePresses deletes the presses that have waited past their interaction's deadline plus
	// grace (an interaction that is not in the store counts from the press's own time), and returns
	// how many. It also forgets the delivery key of each press made grace or longer before now, so that a
	// delivery of it after that is recorded as a new press; the keys are not counted.
	PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (int, error)
}

// RecordOnce records p in s and returns the press to hand to the worker, and whether there is one: p with its Seq; or, for
// a delivery recorded before (ErrDuplicate) whose press still waits, that press as it was recorded, with its own time and
// inputs; or none, for a delivery whose press has been processed since. The press that waits is handed over again because
// the first delivery may not have been: a commit can succeed and its caller see an error, which leaves the press
// unacknowledged, and the worker untold, until the platform sends it again. A worker ignores a Seq it holds already. Any
// other error is RecordPress's or GetPress's, and nothing is to be handed over.
func RecordOnce(ctx context.Context, s Presses, p Press, limits PressLimits) (Press, bool, error) {
	seq, err := s.RecordPress(ctx, p, limits)
	switch {
	case err == nil:
		p.Seq = seq
		return p, true, nil
	case !errors.Is(err, ErrDuplicate):
		return Press{}, false, err
	case seq == 0:
		return Press{}, false, nil
	}
	waiting, err := s.GetPress(ctx, seq)
	switch {
	case errors.Is(err, ErrNotFound): // processed since RecordPress looked
		return Press{}, false, nil
	case err != nil:
		return Press{}, false, fmt.Errorf("read the press %d that a delivery repeats: %w", seq, err)
	}
	return waiting, true, nil
}
