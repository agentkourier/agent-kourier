package session

import "github.com/agentkourier/agent-kourier/api/v1alpha1"

// Reply admission (CONTEXT.md) is the decision what a person's reply in a thread is: a turn for the agent, an
// answer to the question the agent waits on, ignored, or answered with a notice (spec 8.2, 8.4, D38). It is one
// pure function of the facts of the reply, so that the rule is read, tested and changed here and nowhere else:
// Manager.admit gathers the facts from the store and carries out the decision, and the chat intake only
// acknowledges the event and posts the notice.

// AdmissionKind is what a reply was classified as. The set is closed.
type AdmissionKind string

// The kinds of admission.
const (
	// AdmitTurn: the reply is a turn. When the thread has no session yet, it creates one.
	AdmitTurn AdmissionKind = "turn"
	// AdmitAnswer: the reply is the answer to the pending question that takes text replies, Admission.Question. It is
	// queued pinned to the question and is never sent as a turn (D38, 8.4).
	AdmitAnswer AdmissionKind = "answer"
	// AdmitIgnored: nothing is written, queued or touched, and nobody is told. Admission.Ignored says why.
	AdmitIgnored AdmissionKind = "ignored"
	// AdmitNotice: nothing is written; the thread is told, by the notice Admission.Notice.
	AdmitNotice AdmissionKind = "notice"
)

// IgnoreReason is why a reply was ignored.
type IgnoreReason string

// The reasons a reply is ignored.
const (
	// IgnoredNotOwned: courier owns no such thread and the reply may not create one (spec 8.2).
	IgnoredNotOwned IgnoreReason = "not_owned"
	// IgnoredUnmentioned: the mode is mention, the reply does not mention courier, and it answers nothing (D38). It is
	// counted, and an expired thread gets no notice for it either.
	IgnoredUnmentioned IgnoreReason = "unmentioned"
	// IgnoredBindingGone: the session's Binding is gone, the Binding that now owns the channel takes mentioned
	// replies only, and this one does not mention courier. It is not counted: no Binding of the thread is left to
	// count it for.
	IgnoredBindingGone IgnoreReason = "binding_gone"
)

// Admission is the classification of a reply. Only the field of its Kind is set.
type Admission struct {
	Kind     AdmissionKind
	Question string       // AdmitAnswer: the ID of the question the reply answers
	Notice   NoticeKind   // AdmitNotice: NoticeExpired or NoticeBindingGone
	Ignored  IgnoreReason // AdmitIgnored
}

// PendingQuestion is the facts of the question a thread waits on that takes text replies.
type PendingQuestion struct {
	ID     string
	TaskID string // the task that raised it
	Posted bool   // its prompt is in the thread: somebody has seen it
}

// ReplyFacts is everything the decision depends on. Manager.admit fills it in; the zero value is a reply in a
// thread courier does not own.
type ReplyFacts struct {
	// Owned: the thread has a session.
	Owned bool
	// Create: the reply may make the session, and is no reply to be gated: an @mention, a DM, a trigger's start, an
	// alert's prompt.
	Create bool
	// Mentioned: the reply mentions courier, or is in a direct message, where none is needed.
	Mentioned bool
	// BindingGone: the session's Binding has left the config. Nothing else of the session's Binding is known then,
	// and expiry cannot be judged.
	BindingGone bool
	// Expired: the session is past its Binding's threadTTL. Read only when its Binding is live.
	Expired bool
	// Mode is the session's Binding's chat.threadReplies. It decides whenever that Binding is live.
	Mode v1alpha1.ThreadReplies
	// ChannelMode is the chat.threadReplies of the Binding that owns the channel now, empty when none does.
	// It decides, and only when BindingGone: the session's own Binding cannot be asked.
	ChannelMode v1alpha1.ThreadReplies
	// Dialect is the session's Binding's dialect, which says whether it has questions that take text replies.
	Dialect string
	// TaskID is the session's current task, empty when it has none.
	TaskID string
	// Question is the thread's pending question that takes text replies, nil when it has none. Read it only if
	// NeedsQuestion.
	Question *PendingQuestion
}

// TakesTextReplies says whether a Binding whose backend speaks dialect has questions that a typed reply in the
// thread can answer (spec 8.4: the a2a dialect). It is the one definition. What it leaves to data is per question:
// the dialect marks each question it makes (dialect.Interaction.TextReplies), the store keeps the mark (the
// text_replies column) for PendingTextQuestion, and interact checks the question's shape before it decides an answer.
func TakesTextReplies(dialect string) bool { return dialect == string(v1alpha1.DialectA2A) }

// gated says the mode holds this reply back: in mention mode a reply that does not mention courier is no turn (D38).
// The mode is the session's Binding's, and only a session whose Binding is gone has the channel's current Binding's
// instead. Create is no reply and is held to nothing.
func (f ReplyFacts) gated() bool {
	mode := f.Mode
	if f.BindingGone {
		mode = f.ChannelMode
	}
	return mode == v1alpha1.ThreadRepliesMention && !f.Mentioned && !f.Create
}

// NeedsQuestion says the decision reads Question: the reply is held back by the mode in a live thread of a dialect
// that has text questions, so it may be the answer to one. For any other reply Question is not looked at, and
// admit does not read it.
func (f ReplyFacts) NeedsQuestion() bool {
	_, decided := f.decideWithoutQuestion()
	return !decided
}

// Classify decides what the reply is.
func (f ReplyFacts) Classify() Admission {
	if a, decided := f.decideWithoutQuestion(); decided {
		return a
	}
	// A reply that the mode holds back, in a live thread, is the answer to the pending question if the agent waits
	// on it, mention or not, and is queued pinned to it, so that it is decided like a button's press and can never
	// be sent as a turn. A question stays pending until it is answered, refused or swept at its deadline, whether or
	// not the agent still waits on it: one whose task is not the session's (the pause ended, or a new task began)
	// takes no answer, and neither does one whose prompt is not posted (nobody has seen it, and a reply pinned to it
	// would hold the queue until it is). What is typed meanwhile is chatter.
	if q := f.Question; q != nil && q.TaskID == f.TaskID && q.Posted {
		return Admission{Kind: AdmitAnswer, Question: q.ID}
	}
	return ignored(IgnoredUnmentioned)
}

// decideWithoutQuestion is Classify up to the point where only the question can tell, which it reports by false.
func (f ReplyFacts) decideWithoutQuestion() (Admission, bool) {
	switch {
	case !f.Owned && !f.Create:
		return ignored(IgnoredNotOwned), true
	case !f.Owned:
		return Admission{Kind: AdmitTurn}, true
	case f.BindingGone && f.gated():
		return ignored(IgnoredBindingGone), true
	case f.BindingGone:
		return Admission{Kind: AdmitNotice, Notice: NoticeBindingGone}, true
	case f.Expired && f.gated():
		return ignored(IgnoredUnmentioned), true // no expiry notice for a reply that was not asking for anything
	case f.Expired:
		return Admission{Kind: AdmitNotice, Notice: NoticeExpired}, true
	case !f.gated():
		return Admission{Kind: AdmitTurn}, true
	case !TakesTextReplies(f.Dialect):
		return ignored(IgnoredUnmentioned), true
	}
	return Admission{}, false
}

func ignored(why IgnoreReason) Admission { return Admission{Kind: AdmitIgnored, Ignored: why} }
