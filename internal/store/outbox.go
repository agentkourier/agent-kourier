package store

import (
	"context"
	"time"
)

// OutboxEntry is one intended Slack post (spec 8.6).
type OutboxEntry struct {
	// ID is deterministic, derived from the source ID and the role.
	ID string
	// EventID is the source ID: the event ID, or for a digest (D26) the
	// deterministic ID of its window.
	EventID   string
	Role      string // root, reply, digest, or notice
	Channel   string
	ThreadTS  string // empty for a root post
	CreatedAt time.Time
	// MessageRef identifies the posted message; empty until recorded.
	MessageRef string
}

// Outbox persists intended posts and their resulting message refs. Rows are
// never deleted.
type Outbox interface {
	// CreateOutbox stores o idempotently by ID and reports whether it was newly
	// inserted. MessageRef is ignored: rows always start unposted. ID, EventID,
	// Channel and CreatedAt are required (ErrInvalid otherwise).
	CreateOutbox(ctx context.Context, o OutboxEntry) (created bool, err error)
	// RecordMessageRef sets the message ref of an outbox row. Recording the same
	// ref again is a no-op; a different ref over an existing one returns
	// ErrConflict, so recovery never repoints a message. ErrNotFound if the
	// row does not exist.
	RecordMessageRef(ctx context.Context, id, ref string) error
	// ListUnposted returns rows with no message ref, oldest first, for
	// recovery reconciliation.
	ListUnposted(ctx context.Context) ([]OutboxEntry, error)
}
