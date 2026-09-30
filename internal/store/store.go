// Package store defines courier's persistence interfaces and domain types.
//
// The interfaces speak in domain operations, never SQL, so a Postgres
// implementation (spec D15) can sit beside the default SQLite one.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrInvalid is returned when an argument is not a legal value.
	ErrInvalid = errors.New("store: invalid argument")
	// ErrExpired is returned with the row when a session is older than its TTL.
	ErrExpired = errors.New("store: expired")
	// ErrConflict is returned when a row exists but is not in a state that
	// allows the requested change.
	ErrConflict = errors.New("store: conflict")
)

// Store is the full persistence surface, composed of one interface per concern.
type Store interface {
	Events
	Queue
	Outbox
	Sessions
	Interactions
	io.Closer
}

// EventStatus is the processing state of an inbound event.
type EventStatus string

// Event processing states. The queue worker owns retry policy. Claims go
// through Queue.ClaimEvent, which is atomic (UPDATE ... RETURNING on SQLite's
// write pool, FOR UPDATE SKIP LOCKED in Postgres), so callers must not build
// claims from GetEvent plus SetEventStatus.
const (
	EventPending    EventStatus = "pending"
	EventProcessing EventStatus = "processing"
	EventDone       EventStatus = "done"
	EventFailed     EventStatus = "failed"
	EventDead       EventStatus = "dead"
)

// Valid reports whether s is one of the defined statuses.
func (s EventStatus) Valid() bool {
	switch s {
	case EventPending, EventProcessing, EventDone, EventFailed, EventDead:
		return true
	}
	return false
}

// Event is one durable inbound event (spec 9, table events).
type Event struct {
	ID         string
	Kind       string
	BindingKey string
	Payload    []byte
	ReceivedAt time.Time

	// The fields below are owned by the store and queue: AppendEvent ignores
	// them and stores the event as pending with NextAttemptAt = ReceivedAt.
	Status        EventStatus
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
}

// Events persists inbound events.
type Events interface {
	// AppendEvent stores e idempotently by ID. It reports whether the event was
	// newly inserted; a duplicate is a no-op that returns false.
	AppendEvent(ctx context.Context, e Event) (inserted bool, err error)
	// GetEvent returns the event with the given ID, or ErrNotFound.
	GetEvent(ctx context.Context, id string) (Event, error)
	// SetEventStatus records a new processing status and last error text
	// (empty to clear). It returns ErrInvalid for an undefined status and
	// ErrNotFound if the event does not exist.
	SetEventStatus(ctx context.Context, id string, status EventStatus, lastError string) error
}
