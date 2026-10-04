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
	// Opened is when the thread was started, and LastSeen is the latest message of the key that reached it.
	Opened, LastSeen time.Time
	// ClosedAt is when a close message ended the thread, and zero while it is open.
	ClosedAt time.Time
	// CooldownUntil is when the key may start a new turn again. A new turn also needs the thread to be
	// closed: an open thread takes every message of its key until it is.
	CooldownUntil time.Time
}

// TriggerThreads persists the thread of each key of each Binding's botMessage triggers. A row is kept for
// the key after its thread closes, so that the cooldown outlives the close.
type TriggerThreads interface {
	// OpenTriggerThread starts the key's thread as t says, if the key has none yet, or has one that is closed
	// and past its cooldown (CooldownUntil is not after t.Opened); that is one atomic step, so two starts of
	// one key cannot both win. It reports whether t started a thread. If it did not it returns the row that
	// is there, which may be t's own, put there by an earlier delivery of the same message. Binding, KeyHash,
	// Thread and Opened are required, and CooldownUntil may not be before Opened (ErrInvalid otherwise).
	// t.LastSeen and t.ClosedAt are ignored: the row starts open, last seen at Opened.
	OpenTriggerThread(ctx context.Context, t TriggerThread) (current TriggerThread, opened bool, err error)
	// GetTriggerThread returns the key's row, or ErrNotFound.
	GetTriggerThread(ctx context.Context, binding, keyHash string) (TriggerThread, error)
	// ReopenTriggerThread records that the key fired again at at: it moves LastSeen forward, never back, and
	// reopens a closed thread, so that the next close is noted. ErrNotFound if the key has no row.
	ReopenTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) error
	// CloseTriggerThread closes the key's open thread at at, and reports whether it did: a thread that is
	// closed already keeps its first time. ErrNotFound if the key has no row.
	CloseTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (closed bool, err error)
}
