package chat

import (
	"fmt"
	"strings"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// HistoryMessage is a message from a channel's history, as the platform's adapter would have seen it arrive
// live. Replay classifies it.
type HistoryMessage struct {
	// Message is what the adapter would hand the Sink. For an Ignored message only At is set: where it was,
	// so that a listing can show it.
	Message Inbound
	// Own says Agent Kourier itself posted it. The adapter never forwards such a message, whatever a trigger names.
	Own bool
	// Ignored says the adapter would drop it before the Sink: an edit, a deletion or any other subtype that
	// starts nothing, and a bot's message with no content.
	Ignored bool
}

// ReplayOutcome is what the intake would make of a message of the history.
type ReplayOutcome string

// The outcomes of Replay.Classify.
const (
	// ReplayMatched is a bot message that a botMessage trigger took: it would be handed on for a turn.
	ReplayMatched ReplayOutcome = "matched"
	// ReplayUnmatched is a message of a bot that a trigger names, which no trigger took: a match regex, or
	// an extract, failed. It is counted as agentkourier_trigger_unmatched_total.
	ReplayUnmatched ReplayOutcome = "unmatched"
	// ReplayExcluded is a message of a bot that a trigger names, whose match regexes all matched and whose
	// exclude regex turned it away.
	ReplayExcluded ReplayOutcome = "excluded"
	// ReplayOtherBot is a message of a bot that no botMessage trigger names: the adapter drops it.
	ReplayOtherBot ReplayOutcome = "other_bot"
	// ReplayOwn is Agent Kourier's own post, which is never forwarded.
	ReplayOwn ReplayOutcome = "own"
	// ReplayPerson is a person's message, which no botMessage trigger sees: only a mention, or a reply
	// in a thread Agent Kourier owns, makes a turn of it.
	ReplayPerson ReplayOutcome = "person"
	// ReplayIgnored is a message the intake leaves alone whoever sent it: one the adapter drops, and a bot's
	// reply inside a thread.
	ReplayIgnored ReplayOutcome = "ignored"
)

// ReplayVerdict is what Replay.Classify makes of one message.
type ReplayVerdict struct {
	Outcome ReplayOutcome
	// Trigger is the index in the Binding's chat.triggers of the trigger that matched. Meaningful only for
	// ReplayMatched.
	Trigger int
	// Fields are the values the matching trigger extracted. Set only for ReplayMatched.
	Fields map[string]string
	// Why says, for ReplayUnmatched and ReplayExcluded, where each trigger of the message's bot turned
	// it away, at the first of its regexes that failed (match and exclude regexes are tried in the order of
	// their field names): "trigger 0: match link; trigger 2: extract key".
	Why string
}

// Replay applies the botMessage triggers of one Binding to messages that were posted earlier, as the intake
// does to live ones, for the dry run (CT-6). It uses the intake's own matcher, so it cannot disagree with it.
type Replay struct {
	table triggerTable
}

// NewReplay compiles the botMessage triggers of b, which the loader has defaulted and validated, as the
// intake does. A trigger that does not compile is an error here, where the intake would log it and skip it: a
// dry run over a trigger that never matches would mislead.
func NewReplay(b *v1alpha1.Binding) (*Replay, error) {
	var table triggerTable
	for i := range b.Spec.Chat.Triggers {
		tr := &b.Spec.Chat.Triggers[i]
		if tr.Type != v1alpha1.TriggerBotMessage || tr.From == nil {
			continue
		}
		bt, err := compileTrigger(i, tr)
		if err != nil {
			return nil, fmt.Errorf("trigger %d: %w", i, err)
		}
		table = append(table, bt)
	}
	return &Replay{table: table}, nil
}

// Classify says what the intake would make of h.
//   - A message the adapter drops, and Agent Kourier's own post, never reach a trigger.
//   - A person's message is no input of a botMessage trigger.
//   - A bot that no trigger names is dropped by the adapter; a bot's reply inside a thread is ignored by the intake.
//   - Any other bot message is tried against the triggers in order, by the intake's own matcher: the first match
//     wins. Where none matches, an exclude that turned it away makes it excluded, and anything else unmatched.
func (r *Replay) Classify(h HistoryMessage) ReplayVerdict {
	m := h.Message
	switch {
	case h.Ignored:
		return ReplayVerdict{Outcome: ReplayIgnored}
	case h.Own:
		return ReplayVerdict{Outcome: ReplayOwn}
	case m.Bot == nil:
		return ReplayVerdict{Outcome: ReplayPerson}
	case !r.names(m.Bot.ID):
		return ReplayVerdict{Outcome: ReplayOtherBot}
	case m.At.Thread != "" && m.At.Thread != m.At.ID:
		return ReplayVerdict{Outcome: ReplayIgnored}
	}
	if t, fields, ok := r.table.match(m); ok {
		return ReplayVerdict{Outcome: ReplayMatched, Trigger: t.index, Fields: fields}
	}
	outcome := ReplayUnmatched
	var why []string
	for i := range r.table {
		t := &r.table[i]
		if t.from.ID != m.Bot.ID {
			continue
		}
		_, missed := t.check(m)
		if missed.kind == missBot { // the ID is the trigger's and the kind is not
			missed.detail = "kind"
		}
		if missed.kind == missExclude {
			outcome = ReplayExcluded
		}
		step := map[missKind]string{missBot: "sender", missMatch: "match", missExclude: "exclude", missExtract: "extract"}[missed.kind]
		why = append(why, fmt.Sprintf("trigger %d: %s %s", t.index, step, missed.detail))
	}
	return ReplayVerdict{Outcome: outcome, Why: strings.Join(why, "; ")}
}

// names says whether a trigger of the table names the bot, by ID (D36), as Intake.AcceptsBot does. A bot of the right ID
// and the wrong kind is named, and then unmatched, with the kind as the reason, as it is live.
func (r *Replay) names(botID string) bool {
	for i := range r.table {
		if r.table[i].from.ID == botID {
			return true
		}
	}
	return false
}
