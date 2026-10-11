package store

import (
	"context"
	"time"
)

// Verdict is what a person says of a message of an investigation: spec 8.7.
type Verdict string

// The verdicts, the closed set of the verdict label of agentkourier_investigation_feedback_total.
const (
	VerdictUseful    Verdict = "useful"
	VerdictNotUseful Verdict = "not_useful"
)

// Valid reports whether v is one of the defined verdicts.
func (v Verdict) Valid() bool { return v == VerdictUseful || v == VerdictNotUseful }

// FeedbackKind says what the rated message is: the final message of an investigation that nobody asked for, which a
// trigger or an alert started, or the answer to a person's message in a thread.
type FeedbackKind string

// The kinds of rated message.
const (
	FeedbackInvestigation FeedbackKind = "investigation"
	FeedbackAnswer        FeedbackKind = "answer"
)

// Valid reports whether k is one of the defined kinds.
func (k FeedbackKind) Valid() bool { return k == FeedbackInvestigation || k == FeedbackAnswer }

// FeedbackVote is one person's verdict on one message of one session: the row of the feedback table (spec 9). It holds
// identifiers, a name and a verdict, and no text a person wrote: Agent Kourier asks for none.
type FeedbackVote struct {
	// Key is the session, and Turn the turn whose output is the rated message (its outbox source). Together with User
	// they are the vote's identity: a person has one vote on a message, and a later one replaces it.
	Key  ConversationKey
	Turn string
	// User is the platform's ID of who voted.
	User string
	// Binding is the namespace/name of the Binding, and Agent the agent's namespace/name on its backend. Alert is the
	// alert name the thread was started for, or empty for a thread no alert started. All three are kept as they were at
	// the person's first vote.
	Binding, Agent, Alert string
	Kind                  FeedbackKind
	Verdict               Verdict
	// At is when the latest vote was cast, and is required when a vote is recorded. First is when the person first
	// voted on the message: the store sets it, and ignores it on a write.
	At, First time.Time
}

// Feedback persists the verdicts people give. The rows are kept: no retention purge deletes one (docs/reference/stored-data.md).
type Feedback interface {
	// RecordVote stores v, and returns the verdict this person's vote on this message had before it, which is
	// empty for a first vote. A vote with another verdict than the one stored replaces it, and At is then the time of
	// the new verdict; one with the same verdict changes nothing, and returns that verdict: a caller that sees it
	// equal to v.Verdict knows nothing changed. Votes of one person recorded at once are applied one after the other, and
	// exactly one of them reports no previous verdict. Key, Turn, User, Binding, Agent, a valid Kind and Verdict, and an At after the
	// Unix epoch are required (ErrInvalid otherwise).
	RecordVote(ctx context.Context, v FeedbackVote) (previous Verdict, err error)
	// ListVotes returns the votes of the session key, oldest first vote first, with First set. A session nobody voted
	// in has none.
	ListVotes(ctx context.Context, key ConversationKey) ([]FeedbackVote, error)
}
