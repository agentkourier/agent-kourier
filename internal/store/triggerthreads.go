package store

import (
	"context"
	"time"
)

// TriggerThread is the thread courier opened for one key of a botMessage trigger (spec 8.2): the row that
// makes a repeat of the key a note in that thread, and a re-fire after a close, inside the cooldown, the same
// thread again.
type TriggerThread struct {
	// Binding is the namespace/name of the Binding whose trigger the key belongs to.
	Binding string
	// KeyHash identifies the key. The key is untrusted text out of a bot's message, so the caller hashes it
	// (together with whatever else scopes a key, such as the channel) and the store never holds the key
	// itself. The store reads the hash as an opaque string.
	KeyHash string
	// Thread is the thread courier opened: the ts of the message that started it.
	Thread string
	// Opened is when the thread was reserved, and LastSeen is the latest message of the key that reached it.
	Opened, LastSeen time.Time
	// ClosedAt is when a close message ended the thread, and zero while it is open.
	ClosedAt time.Time
	// CooldownUntil is when the key may start a new turn again, and it is zero until the thread's turn has
	// started (MarkTriggerStarted): a thread that only reserved its key, whose turn was never started, has no
	// cooldown running and no investigation behind it. A new turn also needs the thread to be closed: an open
	// thread takes every message of its key until it is.
	CooldownUntil time.Time
}

// Started says whether the thread's turn started, so that its cooldown runs.
func (t TriggerThread) Started() bool { return !t.CooldownUntil.IsZero() }

// TriggerThreads persists the thread of each key of each Binding's botMessage triggers. A row is kept for
// the key after its thread closes, so that the cooldown outlives the close.
//
// A thread has two steps, so that a turn that could not start does not hold its key: OpenTriggerThread reserves
// the key for a message, and MarkTriggerStarted, when that message's turn has started, starts the cooldown. A
// reservation whose turn never started is taken over by a later message of the key, once it is stale.
type TriggerThreads interface {
	// OpenTriggerThread reserves the key's thread for t.Thread, if the key has none yet, or has one that is
	// closed and past its cooldown (CooldownUntil is not after t.Opened), or has one that never started its
	// turn, is another message's and was opened before staleBefore (the zero time takes over none); that is
	// one atomic step, so two starts of one key cannot both win. The staleness is what tells a turn that is
	// starting from one that never will. It reports whether t reserved the thread. If it did not it returns the
	// row that is there, which may be t's own, put there by an earlier delivery of the same message. Binding,
	// KeyHash, Thread and Opened are required (ErrInvalid otherwise). The row starts open, last seen at Opened,
	// with no cooldown: the other fields of t are ignored.
	OpenTriggerThread(ctx context.Context, t TriggerThread, staleBefore time.Time) (current TriggerThread, opened bool, err error)
	// MarkTriggerStarted records that the turn of the key's thread started, and that the key starts no new
	// turn until cooldownUntil. It is the same to mark a thread that is marked already. ErrNotFound if the key
	// has no row, and ErrConflict if its thread is another message's, which took the key over: the turn is
	// not the key's. cooldownUntil is required, and after the Unix epoch (ErrInvalid otherwise).
	MarkTriggerStarted(ctx context.Context, binding, keyHash, thread string, cooldownUntil time.Time) error
	// GetTriggerThread returns the key's row, or ErrNotFound.
	GetTriggerThread(ctx context.Context, binding, keyHash string) (TriggerThread, error)
	// ReopenTriggerThread records that the key fired again at at: it moves LastSeen forward, never back, and
	// reopens a closed thread, so that the next close is noted. ErrNotFound if the key has no row.
	ReopenTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) error
	// CloseTriggerThread closes the key's open thread at at, and reports whether it did: a thread that is
	// closed already keeps its first time. ErrNotFound if the key has no row.
	CloseTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (closed bool, err error)
}
