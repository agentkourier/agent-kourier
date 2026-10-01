package render

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// Outcome is how a prompt ended.
type Outcome int

// The outcomes. A pause courier cannot show has no prompt to replace, so it has none.
const (
	// Answered: a person submitted a complete answer.
	Answered Outcome = iota
	// TimedOut: nobody answered by the deadline.
	TimedOut
	// Cancelled: the task was cancelled, by Stop or because it ended some other way.
	Cancelled
)

// Ending is what a prompt's replacement says about how it ended.
type Ending struct {
	Outcome Outcome
	// By is the Slack user who decided (Answered) or stopped it (Cancelled), as courier verified it,
	// or "". It is not agent text, and is shown as a mention only if it has the shape of a user ID.
	By string
	// At is when it ended.
	At time.Time
	// Hint is the prompt's header text, and Subagent the sub-agent that raised a nested pause, or "".
	Hint, Subagent string
	// Questions and Answers are what was asked and, for Answered, the chosen choices or the free
	// text of each question, in order.
	Questions []dialect.Question
	Answers   [][]string
}

// userID is the shape of a Slack user ID.
var userID = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)

// Replacement is what a prompt's message says once it has ended, as the top-level text and blocks
// of an update: who decided and when, a summary of the answer, and no input or button. The only
// text the agent wrote in it (the hint, the sub-agent, the questions and the answers) goes in
// mrkdwn sections and context blocks, through Text; the mention and the date are built here, from a
// verified user ID and a time. No field is plain_text, so no Plain flavour is needed.
func Replacement(e Ending) (text string, blocks []map[string]any) {
	verb := [...]string{Answered: "Answered", TimedOut: "Timed out", Cancelled: "Cancelled"}[e.Outcome]
	status := "*" + verb + "*"
	if e.By != "" {
		who := "someone"
		if userID.MatchString(e.By) {
			who = "<@" + e.By + ">"
		}
		status += " by " + who
	}
	if !e.At.IsZero() {
		status += " " + fmt.Sprintf("<!date^%d^{date_short_pretty} at {time}|%s>", e.At.Unix(), e.At.UTC().Format("2006-01-02 15:04 UTC"))
	}
	blocks = []map[string]any{{"type": "section", "text": mrkdwn(status)}}
	var context []map[string]any
	if e.Subagent != "" {
		context = append(context, mrkdwn("Raised by sub-agent *"+Text(e.Subagent, maxOptionText)+"*"))
	}
	if hint := Text(e.Hint, maxHeader); hint != "" {
		context = append(context, mrkdwn(hint))
	}
	if len(context) > 0 {
		blocks = append(blocks, map[string]any{"type": "context", "elements": context})
	}
	if e.Outcome == Answered {
		blocks = append(blocks, answerSections(e.Questions, e.Answers)...)
	}
	return verb, blocks
}

func mrkdwn(s string) map[string]any { return map[string]any{"type": "mrkdwn", "text": s} }

// answerSections is a line for each question and its answer, in sections of at most 3000 characters.
func answerSections(qs []dialect.Question, answers [][]string) []map[string]any {
	var sections []map[string]any
	var b strings.Builder
	for i, q := range qs {
		var given []string
		if i < len(answers) {
			given = answers[i]
		}
		line := "*" + cmp.Or(Text(q.Text, maxListLine/2), "Question "+strconv.Itoa(i+1)) + "* " + cmp.Or(Text(strings.Join(given, ", "), 2*maxListLine), "(no answer)") + "\n"
		if b.Len() > 0 && units(b.String())+units(line) > maxSectionText {
			sections = append(sections, map[string]any{"type": "section", "text": mrkdwn(b.String())})
			b.Reset()
		}
		b.WriteString(line)
	}
	if b.Len() > 0 {
		sections = append(sections, map[string]any{"type": "section", "text": mrkdwn(b.String())})
	}
	return sections
}
