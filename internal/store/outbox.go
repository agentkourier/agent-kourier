package store

import (
	"context"
	"time"
)

// OutboxRole is the role of an intended post (spec 8.6).
type OutboxRole string

// Outbox post roles.
const (
	OutboxRoot   OutboxRole = "root"
	OutboxReply  OutboxRole = "reply"
	OutboxDigest OutboxRole = "digest"
	OutboxNotice OutboxRole = "notice"
)

// Valid reports whether r is one of the defined roles.
func (r OutboxRole) Valid() bool {
	switch r {
	case OutboxRoot, OutboxReply, OutboxDigest, OutboxNotice:
		return true
	}
	return false
}

// OutboxEntry is one intended Slack post (spec 8.6).
type OutboxEntry struct {
	// ID is deterministic, derived from the source ID and the role.
	ID string
	// EventID is the source ID: the event ID, or for a digest (D26) the
	// deterministic ID of its window.
	EventID   string
	Role      OutboxRole
	Channel   string
	ThreadTS  string // empty for a root post
	CreatedAt time.Time
	// MessageRef identifies the posted message; empty until recorded.
	MessageRef string
	// Payload is the message to post, as JSON the store never interprets. It is cleared when
	// the ref is recorded: from then on the text lives in Slack, not in the database too.
	Payload string
	// AbandonedAt is when courier gave up on posting the message; zero if it did not (see
	// AbandonOutbox).
	AbandonedAt time.Time
}

// Outbox persists intended posts and their resulting message refs. Rows are
// never deleted.
type Outbox interface {
	// CreateOutbox stores o idempotently by ID and reports whether it was newly
	// inserted. MessageRef is ignored: rows always start unposted. ID, EventID,
	// a valid Role, Channel and CreatedAt are required (ErrInvalid otherwise).
	CreateOutbox(ctx context.Context, o OutboxEntry) (created bool, err error)
	// RecordMessageRef sets the message ref of an outbox row and clears its Payload.
	// Recording the same ref again is a no-op; a different ref over an existing one
	// returns ErrConflict, so recovery never repoints a message. ErrNotFound if the
	// row does not exist.
	RecordMessageRef(ctx context.Context, id, ref string) error
	// GetOutbox returns the row with the given ID, or ErrNotFound.
	GetOutbox(ctx context.Context, id string) (OutboxEntry, error)
	// AbandonOutbox gives up on an unposted row that will never post: it records at and clears
	// the Payload, so an agent's text is not kept for a post that will not happen. It reports
	// whether the row was newly abandoned, and an abandoned row stays so, with its first time.
	// ErrNotFound if the row does not exist; ErrConflict if it already has a message ref.
	AbandonOutbox(ctx context.Context, id string, at time.Time) (abandoned bool, err error)
	// AbandonOutboxBefore abandons, as of at, every row created before the cutoff that has a
	// Payload and no message ref and is not abandoned, and returns their IDs: the retention
	// limit on unposted text. A row with no Payload, a stream's, is left alone.
	AbandonOutboxBefore(ctx context.Context, before, at time.Time) (ids []string, err error)
	// ListUnposted returns rows with no message ref that are not abandoned, oldest first, for
	// recovery reconciliation.
	ListUnposted(ctx context.Context) ([]OutboxEntry, error)
}
