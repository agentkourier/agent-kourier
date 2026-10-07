package store

import (
	"context"
	"time"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// AuditKind is the category of an audited action (spec G8, section 10).
type AuditKind string

// Audit kinds.
const (
	AuditEvent               AuditKind = "event"
	AuditAgentCall           AuditKind = "agent_call"
	AuditInteractionDecision AuditKind = "interaction_decision"
	AuditConfigChange        AuditKind = "config_change"
)

// Valid reports whether k is one of the defined kinds.
func (k AuditKind) Valid() bool {
	switch k {
	case AuditEvent, AuditAgentCall, AuditInteractionDecision, AuditConfigChange:
		return true
	}
	return false
}

// MaxAuditOutcome is the longest Outcome, in bytes.
const MaxAuditOutcome = 200

// AuditEntry records who did what to which subject, and how it ended.
//
// It holds identifiers and a short outcome only. Never put a payload, message
// text, token, or any Secret content in it: the audit table is read by people
// and exported (spec section 10). As a backstop, AppendAudit passes every text
// field except Email through redact.IdentifierText (see Redacted), so what is stored is
// normalized, on one line.
type AuditEntry struct {
	// Sequence is assigned by the store on append.
	Sequence int64
	At       time.Time
	Kind     AuditKind
	User     string // the platform's user ID; empty when the actor is not a person
	// Email is the user's email as the platform reports it. Optional: users.info can
	// fail, and append-only rows cannot be backfilled.
	Email    string
	Identity string // Binding service identity the action ran as; may be empty
	Binding  string
	// Subject identifies what was acted on: an event, task or interaction ID,
	// or a resource name.
	Subject string
	// Outcome is a short result such as "accepted" or "denied: not a member",
	// at most MaxAuditOutcome bytes.
	Outcome string
}

// Redacted returns e with redact.IdentifierText applied to every text field: User, Identity, Binding,
// Subject and Outcome, which come back normalized (the invisible characters deleted) and on one line,
// with the tokens taken out. Kind is a validated enum, and Email is an identity field by design (spec
// 8.5), so both stay as they are. Every Audit implementation must store the result of Redacted, never
// e itself: the store is the last line of defence behind the audit writer.
func (e AuditEntry) Redacted() AuditEntry {
	e.User = redact.IdentifierText(e.User)
	e.Identity = redact.IdentifierText(e.Identity)
	e.Binding = redact.IdentifierText(e.Binding)
	e.Subject = redact.IdentifierText(e.Subject)
	e.Outcome = redact.IdentifierText(e.Outcome)
	return e
}

// Audit is an append-only log. There is no update or delete; the one way entries leave it is the
// retention purge (AuditRetention).
type Audit interface {
	// AppendAudit adds e to the log. Kind, At, Subject and Outcome are
	// required, and Outcome must fit MaxAuditOutcome (ErrInvalid otherwise), measured after
	// redaction, on the text that is kept. The store assigns Sequence; the value in e is ignored.
	// A reader that pages with ListAudit must never miss an entry appended while it pages: an
	// entry becomes visible only after every entry with a lower Sequence is.
	AppendAudit(ctx context.Context, e AuditEntry) error
	// ListAudit returns entries with At at or after since (zero for no lower
	// bound) and Sequence above afterSeq, in append order, at most limit of
	// them (1 to 1000, ErrInvalid otherwise). To page, pass the last returned
	// Sequence as afterSeq.
	ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) ([]AuditEntry, error)
}

// AuditRetention removes audit entries that are past the operator's retention. It is a separate interface
// from Audit so that the writer of the log, which only appends, cannot reach it.
type AuditRetention interface {
	// PurgeAudit deletes at most limit of the oldest entries whose At is before the cutoff, oldest first, and
	// returns how many it deleted; a count below limit means nothing older is left. The caller runs it
	// again while it returns limit, so that no single call holds the log for long (Postgres's statement
	// timeout, SQLite's one writer). It is the only way an entry is removed: the log stays append-only
	// to every other path of the application. before must not be zero and limit must be 1 to MaxLimit
	// (ErrInvalid otherwise). It writes no entry of its own.
	PurgeAudit(ctx context.Context, before time.Time, limit int) (int, error)
}
