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
// field except Email through redact.Secrets (see Redacted).
type AuditEntry struct {
	// Sequence is assigned by the store on append.
	Sequence int64
	At       time.Time
	Kind     AuditKind
	User     string // Slack user ID; empty when the actor is not a person
	// Email is the user's email as Slack reports it. Optional: users.info can
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

// Redacted returns e with redact.Secrets applied to every free-text field: User,
// Identity, Binding, Subject and Outcome. Kind is a validated enum, and Email is
// an identity field by design (spec 8.5), so both stay as they are. Every Audit
// implementation must store the result of Redacted, never e itself: the store
// is the last line of defence behind the audit writer.
func (e AuditEntry) Redacted() AuditEntry {
	e.User = redact.Secrets(e.User)
	e.Identity = redact.Secrets(e.Identity)
	e.Binding = redact.Secrets(e.Binding)
	e.Subject = redact.Secrets(e.Subject)
	e.Outcome = redact.Secrets(e.Outcome)
	return e
}

// Audit is an append-only log. There is no update or delete.
type Audit interface {
	// AppendAudit adds e to the log. Kind, At, Subject and Outcome are
	// required, and Outcome must fit MaxAuditOutcome (ErrInvalid otherwise).
	// The store assigns Sequence; the value in e is ignored.
	AppendAudit(ctx context.Context, e AuditEntry) error
	// ListAudit returns entries with At at or after since (zero for no lower
	// bound) and Sequence above afterSeq, in append order, at most limit of
	// them (1 to 1000, ErrInvalid otherwise). To page, pass the last returned
	// Sequence as afterSeq.
	ListAudit(ctx context.Context, since time.Time, afterSeq int64, limit int) ([]AuditEntry, error)
}
