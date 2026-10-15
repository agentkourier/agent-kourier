package interact

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// words are the fixed texts told to a person alone about a press, in the words of what was asked: a question is answered, a
// request to run tools is decided. None holds anything the agent wrote. They are picked by the kind of the decoded request
// (wordsOf), never by the row's kind, which says "nested" for an approval that a sub-agent raised.
type words struct {
	gone, policyDenied, notReady, notMember, late, invalid, timedOut, closed, gaveUp string
	// notApprover is told to a member who is in none of the Binding's approver groups. Only an approval has it: a question
	// is not gated by groups. It names no group.
	notApprover string
	// groupsBroken is told to anyone whose press needed the approver-group check when the platform said the check can never
	// succeed until an operator fixes the Binding's groups (chat.ErrGroupsMisconfigured). Only an approval has it. It is true
	// of the one told, member of a group or not: their decision could not be checked, so it was not taken. It names no group
	// and holds nothing from the platform's error.
	groupsBroken string
	// unrecorded is told to a person whose decision the audit cannot write (the row cannot be built, which no retry mends), so it
	// was not taken. It holds nothing from the error, and the press is not tried again.
	unrecorded string
	// firstNone is told when someone else decided first and is not named; firstWords follows the mention of the one who did.
	firstNone, firstWords string
}

// wordsOf is the wording for a request of kind k. Every kind but a tool approval is worded as a question.
func wordsOf(k dialect.Kind) words {
	if k == dialect.ToolApproval {
		return words{
			gone:         "This request is no longer available.",
			policyDenied: "This request was denied because this Binding does not allow approvals.",
			notReady:     "This request is not ready yet. Please press again in a moment.",
			notMember:    "Only members of this channel can decide this request.",
			notApprover:  "Only approvers of this Binding can decide this request.",
			groupsBroken: "Your decision could not be checked, because this Binding's approver groups cannot be read, so it was not taken. An operator must fix this.",
			unrecorded:   "Your decision could not be recorded, so it was not taken. An operator must fix this.",
			late:         "This request has timed out, so your decision was not taken.",
			invalid:      "That decision could not be used. Please check it and press again.",
			timedOut:     "This request has timed out.",
			closed:       "This request is closed.",
			gaveUp:       "Couldn't process your decision. Please press again.",
			firstNone:    "Someone else decided this request first.",
			firstWords:   "decided this request first.",
		}
	}
	return words{
		gone: textGone, notReady: textNotReady, notMember: textNotMember, late: textLate, invalid: textInvalid,
		timedOut: textTimedOut, closed: textClosed, gaveUp: textGaveUp, unrecorded: textUnrecorded, firstNone: textFirstNone, firstWords: textFirstWords,
	}
}

// requestKind is the kind of the request stored in a row, or the zero kind (worded as a question) if there is none or it cannot
// be read. Only the kind is read: the pipeline needs the wording before it has checked who pressed and decoded the rest.
func requestKind(row store.Interaction) dialect.Kind {
	var k struct {
		Kind dialect.Kind `json:"kind"`
	}
	if json.Unmarshal(row.Request, &k) != nil {
		return ""
	}
	return k.Kind
}

// incompleteText names the questions that need an answer, by number only.
func incompleteText(problems []Problem) string {
	var unanswered, invalid []string
	for _, pr := range problems {
		if pr.Kind == Invalid {
			invalid = append(invalid, strconv.Itoa(pr.Question+1))
		} else {
			unanswered = append(unanswered, strconv.Itoa(pr.Question+1))
		}
	}
	switch {
	case len(invalid) > 0:
		return "Question " + strings.Join(invalid, ", ") + " could not be read. Please check it and press Submit again."
	default:
		return "Please answer question " + strings.Join(unanswered, ", ") + ", then press Submit again."
	}
}

// incompleteToolsText names the tools that need a verdict, by number only (the number the prompt shows beside each tool),
// and never says what was chosen for any.
func incompleteToolsText(problems []Problem) string {
	var undecided, invalid []string
	for _, pr := range problems {
		if pr.Kind == Invalid {
			invalid = append(invalid, strconv.Itoa(pr.Question+1))
		} else {
			undecided = append(undecided, strconv.Itoa(pr.Question+1))
		}
	}
	switch {
	case len(invalid) > 0:
		return "Tool " + strings.Join(invalid, ", ") + " could not be read. Please check it and press Submit again."
	default:
		return "Please choose Approve or Deny for tool " + strings.Join(undecided, ", ") + ", then press Submit again."
	}
}
