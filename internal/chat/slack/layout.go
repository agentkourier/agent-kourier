package slack

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// This file is the Block Kit layout of the neutral model: what a Message, a Prompt and an Ending look like
// in Slack, and every rule of Slack's that decides it (which control a question gets, the numbered list a
// choice that is too long falls back to, the 50 blocks of a message). The neutral model says what is
// shown; this says how Slack shows it.

// Slack's limits, in UTF-16 units. UNCONFIRMED(WP-04): documented, not measured.
const (
	maxBlocks        = 50    // blocks in a message
	maxHeader        = 150   // a header block's text
	maxLabel         = 2000  // an input block's label
	maxSection       = 3000  // a section block's text
	maxContext       = 2000  // a context block's text element
	maxOption        = 75    // an option's text, and a button's
	maxInlineOptions = 10    // options a radio-button group or a set of checkboxes holds
	maxSelectOptions = 100   // options a select holds
	maxListLine      = 200   // one choice in the numbered list a prompt falls back to, with its number
	maxMarkdown      = 12000 // a markdown block's text, and across all of a message's
	maxValue         = 2000  // a button's value
	maxToolName      = 100   // a tool's name in a status line
	maxToolArgs      = 190   // the arguments in a status line, which with its words is at most 300
	maxStepText      = 256   // a task_update's title, and its details: documented as 256 characters
	maxSubagentLine  = 100   // the line that names a sub-agent, with its words
	maxAnswerLine    = 600   // a question and its answer in an ending
	maxQuestionInEnd = 100   // a question's text in the ending's summary of it
	maxActions       = 25    // elements in an actions block
	maxFields        = 10    // fields in a section block
	defaultHeader    = "The agent needs your input"
	noText           = "(empty message)"
)

// ackReaction is the reaction Agent Kourier puts on an accepted message while it works, when a Binding names none.
const ackReaction = "hourglass_flowing_sand"

// caps is what Slack shows. The limits are those of the place's carrier, which is the whole field: the tool
// name and its arguments are shown in one status line, so both have its limit; a choice is shown on a
// numbered line, and an answer on the line of its question. The name of a tool awaiting approval has the same
// limit, and its arguments, which are shown as preformatted text, have maxApprovalArgs.
var caps = chat.Caps{
	Streams: true, Steps: true, Ephemeral: true, Edits: true, Inputs: true, Busy: true, MaxText: maxSection, AckReaction: ackReaction,
	Limits: map[chat.Where]int{
		chat.WhereFallback: maxSection, chat.WhereHeader: maxHeader, chat.WhereBody: maxMarkdown, chat.WhereStream: maxMarkdown,
		chat.WhereContext: maxContext, chat.WhereNote: maxSection, chat.WhereToolName: 300, chat.WhereToolArg: 300, chat.WhereToolArgs: maxApprovalArgs,
		chat.WhereStepTitle: maxStepText, chat.WhereStepDetail: maxStepText,
		chat.WhereButton: maxOption, chat.WhereHint: maxHeader, chat.WhereSubagent: maxSubagentLine, chat.WhereQuestion: maxLabel,
		chat.WhereChoice: maxListLine, chat.WhereAnswer: maxAnswerLine,
	},
}

func plainObject(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s} }

// mrkdwnObject is a mrkdwn text object, verbatim: Slack's default, verbatim false, links channel names and
// parses some mentions in it whatever the escaping did.
func mrkdwnObject(s string) map[string]any {
	return map[string]any{"type": "mrkdwn", "text": s, "verbatim": true}
}

func section(s string) map[string]any {
	return map[string]any{"type": "section", "text": plainObject(s)}
}

func contextLine(s string) map[string]any {
	return map[string]any{"type": "context", "elements": []map[string]any{plainObject(s)}}
}

// content is the blocks as the JSON a Content holds, with the text beside them.
func content(fallback string, blocks []map[string]any) (Content, error) {
	raw, err := json.Marshal(blocks)
	if err != nil {
		return Content{}, fmt.Errorf("slack: encode the blocks: %w", err)
	}
	return Content{Text: fallback, Blocks: raw}, nil
}

// renderOutgoing is o as a Slack message.
func renderOutgoing(o chat.Outgoing) (Content, error) {
	if err := o.Validate(); err != nil {
		return Content{}, err
	}
	switch {
	case o.Message != nil:
		return renderMessage(*o.Message)
	case o.Prompt != nil:
		return renderPrompt(*o.Prompt)
	}
	return renderEnding(*o.Ending)
}

// renderMessage is m as a Slack message: its parts as blocks, in order, and its fallback as the top-level
// text, which Slack shows where it cannot show the blocks (a notification). A message that is only a fallback
// has the fallback as a block too, so that what it says is shown as plain text and not as mrkdwn.
func renderMessage(m chat.Message) (Content, error) {
	if err := m.Validate(); err != nil {
		return Content{}, err
	}
	var blocks []map[string]any
	var cards []stepsAt
	var first string // the first text a part has, to stand for a message with no fallback of its own
	note := func(s string) {
		if first == "" {
			first = s
		}
	}
	for i, p := range m.Parts {
		switch p := p.(type) {
		case chat.Steps:
			if len(p.Steps) > 0 {
				cards = append(cards, stepsAt{at: len(blocks), steps: p.Steps})
				note(p.Steps[0].Title)
			}
		case chat.Header:
			if t := text(p.Text, maxHeader, true); t != "" {
				blocks = append(blocks, map[string]any{"type": "header", "text": plainObject(t)})
				note(p.Text)
			}
		case chat.Body:
			if t := markdown(p.Markdown, maxMarkdown); t != "" {
				blocks = append(blocks, map[string]any{"type": "markdown", "text": t})
				note(p.Markdown)
			}
		case chat.Context:
			if t := text(p.Text, maxContext, true); t != "" {
				blocks = append(blocks, contextLine(t))
				note(p.Text)
			}
		case chat.Note:
			if t := text(p.Text, maxSection, false); t != "" {
				blocks = append(blocks, section(t))
				note(p.Text)
			}
		case chat.ToolLine:
			blocks = append(blocks, contextLine(toolLine(p)))
		case chat.Mention:
			blocks = append(blocks, mentionLine(p))
			note(p.Text)
		case chat.Buttons:
			if b := buttons(p, i); b != nil {
				blocks = append(blocks, b)
			}
		}
	}
	said := notBlank(chat.Clean(m.Fallback, false), chat.Clean(first, false))
	if len(blocks) > maxBlocks {
		return Content{}, fmt.Errorf("%w: %d blocks", chat.ErrTooLarge, len(blocks))
	}
	blocks = withCards(blocks, cards)
	if len(blocks) == 0 { // the fallback is the message, and is shown as plain text
		said = cmp.Or(said, noText)
		blocks = append(blocks, section(text(notBlank(said, noText), maxSection, false)))
	}
	fallback := text(said, caps.Limit(chat.WhereFallback), false)
	return content(fallback, blocks)
}

// stepsAt is the steps of a Steps part, and the place in the message's other blocks where their cards go.
type stepsAt struct {
	at    int
	steps []chat.Step
}

// withCards is blocks with the cards of each Steps part at its place. The steps are decoration, and have what room the
// other blocks leave of 50: when there are more than fit, the first are shown, in order, and a line after the last card
// shown counts the rest. A message with no room left shows none.
func withCards(blocks []map[string]any, cards []stepsAt) []map[string]any {
	if len(cards) == 0 {
		return blocks
	}
	room, total := maxBlocks-len(blocks), 0
	for _, c := range cards {
		total += len(c.steps)
	}
	shown := total
	if total > room {
		shown = max(room-1, 0) // one block is the line that counts the rest
	}
	insert := make(map[int][]map[string]any)
	n := 0
	for _, c := range cards {
		for _, st := range c.steps {
			if n == shown {
				break
			}
			insert[c.at] = append(insert[c.at], taskCard(st))
			n++
		}
		if n == shown && shown < total && room > 0 {
			insert[c.at] = append(insert[c.at], contextLine(strconv.Itoa(total-shown)+" more steps are not shown."))
			room = 0 // the line is said once
		}
	}
	out := make([]map[string]any, 0, len(blocks)+n+1)
	for i := range len(blocks) + 1 {
		out = append(out, insert[i]...)
		if i < len(blocks) {
			out = append(out, blocks[i])
		}
	}
	return out
}

// taskCard is a step as a task_card block, which Slack shows as it shows a stream's card (probed 2026-10-21 in a sandbox
// workspace, by chat.postMessage and chat.update, read back through conversations.replies and seen in the web client):
// the status and the output of taskStatus, and the title as stepTitle has it. The details and the output are rich_text,
// whose text elements are literal (approval.go), so the details are cleaned onto one line and cut, and not escaped: an
// entity there would be shown as one. UNCONFIRMED(WP-04): that Slack shows a task_card's rich_text as literally as it
// shows a rich_text block's, and how it reads the title, which is escaped as a task_update's is.
func taskCard(st chat.Step) map[string]any {
	status, output := taskStatus(st.State)
	b := map[string]any{"type": "task_card", "task_id": st.ID, "title": stepTitle(st.Title), "status": string(status)}
	if d := fitWith(chat.Clean(st.Detail, true), caps.Limit(chat.WhereStepDetail), literal); d != "" {
		b["details"] = richText(richSection(d))
	}
	if output != "" {
		b["output"] = richText(richSection(output))
	}
	return b
}

// literal is the escaper of text that is shown as the characters it holds: it changes none of them.
func literal() escaper { return func(r rune) string { return string(r) } }

// notBlank is a if it says something, else b.
func notBlank(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// toolLine is the status line of a tool call, on one line: "Calling name(summary)" while the call is under way, "Ran
// name(summary)" once it has its result, and "name(summary) failed" if the result reports a failure. The caller has
// redacted the summary already.
func toolLine(t chat.ToolLine) string {
	call := text(t.Name, maxToolName, true) + "(" + text(t.Summary, maxToolArgs, true) + ")"
	switch t.State {
	case chat.ToolRan:
		return "Ran " + call
	case chat.ToolFailed:
		return call + " failed"
	}
	return "Calling " + call
}

// stepText is the text of a task_update's title or details: one line, cleaned, cut to limit, and escaped so that it is
// no link, image, mention, broadcast or tag whether Slack reads it as plain text, mrkdwn or markdown.
// UNCONFIRMED(WP-04): which of the three it is. Entities do for "&", "<" and ">", which stops every token of mrkdwn and
// every tag and autolink of markdown. What is left of markdown in one line of text is a link or an image, which needs "]("
// and so has its "(" escaped, a link reference definition, which has to start the text and so has its "[" escaped, and
// a backslash that would undo one of these, which is escaped too. Nothing else is touched, since a visible backslash is the
// price of each of them if the text is plain, and "[redacted]" is in every redacted argument. Emphasis, code, a heading
// and a list may act, which is cosmetic (see the concessions of the Slack subject in platform_test.go).
func stepText(s string, limit int) string {
	return fitWith(chat.Clean(s, true), limit, taskEscaper)
}

// taskEscaper escapes the text of a task, in order (see stepText).
func taskEscaper() escaper {
	var prev rune
	blank := true // nothing but white space so far: a "[" now could open a link reference definition
	return func(r rune) string {
		out := escapeRune(r, false)
		switch {
		case r == '\\':
			out = `\\`
		case r == '[' && blank:
			out = `\[`
		case r == '(' && prev == ']':
			out = `\(`
		}
		prev = r
		if r != ' ' && r != '\t' {
			blank = false
		}
		return out
	}
}

// buttons is an actions block of p's buttons, or nil if there are none: part i of its message, so that its block_id, which a
// press names, is its own in the message and the same whenever the message is rendered. A label cut to nothing reads
// "(blank)": a button with no label is refused.
func buttons(p chat.Buttons, i int) map[string]any {
	var els []map[string]any
	for _, b := range p.Buttons {
		if len(els) == maxActions {
			break
		}
		el := map[string]any{"type": "button", "action_id": b.ID, "value": b.Value, "text": plainObject(cmp.Or(text(b.Label, maxOption, true), "(blank)"))}
		if b.Primary {
			el["style"] = "primary"
		}
		els = append(els, el)
	}
	if len(els) == 0 {
		return nil
	}
	return map[string]any{"type": "actions", "block_id": "buttons" + strconv.Itoa(i), "elements": els}
}

// The block and action IDs of a prompt, which a press names: question i is in the block "q<i>", and its control's
// action ID starts with a letter that says what control it is, so that an answer is read only as the
// control it was asked in.
const (
	submitBlockID  = "submit"
	submitActionID = "submit"

	controlText     = 't' // a free-text input
	controlNumbers  = 'n' // a free-text input beside a numbered list, for choices that cannot be options
	controlRadio    = 'r'
	controlCheck    = 'c'
	controlSelect   = 's'
	controlMultiSel = 'm'
)

// controlElement is the Block Kit type of each control.
var controlElement = map[byte]string{
	controlText: "plain_text_input", controlNumbers: "plain_text_input", controlRadio: "radio_buttons",
	controlCheck: "checkboxes", controlSelect: "static_select", controlMultiSel: "multi_static_select",
}

func questionBlock(i int) string { return "q" + strconv.Itoa(i) }

func actionID(i int, control byte) string { return string(rune(control)) + strconv.Itoa(i) }

// renderPrompt is p as a Slack message: a header, a control for each question, and a Submit button valued
// with the prompt's ID. A prompt that does not fit in 50 blocks is chat.ErrTooLarge; the sub-agent's line and
// the deadline's are left out of one that has no room for them, rather than refusing it.
func renderPrompt(p chat.Prompt) (Content, error) {
	if err := p.Validate(); err != nil {
		return Content{}, err
	}
	if n := chat.Units(p.ID); n > maxValue {
		return Content{}, fmt.Errorf("%w: the prompt ID is %d units; a button's value holds at most %d", chat.ErrInvalid, n, maxValue)
	}
	if len(p.Tools) > 0 {
		return renderToolPrompt(p)
	}
	blocks := []map[string]any{{"type": "header", "text": plainObject(cmp.Or(text(p.Hint, maxHeader, true), defaultHeader))}}
	for i, q := range p.Questions {
		blocks = append(blocks, questionBlocks(i, q)...)
	}
	submit := map[string]any{"type": "actions", "block_id": submitBlockID, "elements": []map[string]any{
		{"type": "button", "action_id": submitActionID, "value": p.ID, "style": "primary", "text": plainObject("Submit")}}}
	blocks = append(blocks, submit)
	if len(blocks) > maxBlocks {
		return Content{}, fmt.Errorf("%w: %d blocks", chat.ErrTooLarge, len(blocks))
	}
	blocks, fallback := withNotices(blocks, p, "The agent needs your input.")
	return content(fallback, blocks)
}

// withNotices adds the lines that a prompt of either kind has to blocks, which are the header, the controls and
// the button block last: the sub-agent's line under the header, and the deadline's above the buttons, each only if
// there is room for it in 50 blocks. The deadline is also said in the fallback, which begins as fallback.
func withNotices(blocks []map[string]any, p chat.Prompt, fallback string) ([]map[string]any, string) {
	if p.Subagent != "" && len(blocks) < maxBlocks { // under the header
		blocks = append(blocks[:1:1], append([]map[string]any{contextLine(subagentLine(p.Subagent))}, blocks[1:]...)...)
	}
	if !p.Deadline.IsZero() {
		fallback += " Answer by " + utc(p.Deadline) + "."
		if len(blocks) < maxBlocks { // above the buttons, which stay last
			hint := map[string]any{"type": "context", "elements": []map[string]any{mrkdwnObject("Answer by " + dateToken(p.Deadline))}}
			blocks = append(blocks[:len(blocks)-1:len(blocks)-1], hint, blocks[len(blocks)-1])
		}
	}
	return blocks, fallback
}

func subagentLine(name string) string {
	return within("Raised by sub-agent ", name, "", maxSubagentLine, true)
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// dateToken is a time as Slack shows it in the reader's zone, with UTC as the fallback after the bar. It is
// built here, from a time, and never from text.
func dateToken(t time.Time) string {
	return fmt.Sprintf("<!date^%d^{date_short_pretty} at {time}|%s>", t.Unix(), utc(t))
}

// questionBlocks is the blocks of question i: an input for free text, one control for choices that fit as
// options, and for those that do not (more than 100, or one blank or too long for an option) a numbered list
// of every choice with an input for the numbers.
func questionBlocks(i int, q chat.Question) []map[string]any {
	label := cmp.Or(text(q.Text, maxLabel, true), "Question "+strconv.Itoa(i+1))
	input := func(control byte, label string, element map[string]any) map[string]any {
		element["type"] = controlElement[control]
		element["action_id"] = actionID(i, control)
		return map[string]any{"type": "input", "block_id": questionBlock(i), "dispatch_action": false, "optional": false,
			"label": plainObject(label), "element": element}
	}
	if len(q.Choices) == 0 {
		return []map[string]any{input(controlText, label, map[string]any{})}
	}
	opts, ok := options(q.Choices)
	if !ok {
		ask, example := "Your answer: the number", "For example: 2"
		if q.Multiple {
			ask, example = "Your answer: the number or numbers, separated by commas", "For example: 2 or 1, 3"
		}
		blocks := numberedList(label, q.Choices)
		return append(blocks, input(controlNumbers, ask, map[string]any{"placeholder": plainObject(example)}))
	}
	control := byte(controlRadio)
	switch {
	case q.Multiple && len(opts) > maxInlineOptions:
		control = controlMultiSel
	case q.Multiple:
		control = controlCheck
	case len(opts) > maxInlineOptions:
		control = controlSelect
	}
	el := map[string]any{"options": opts}
	if control == controlSelect || control == controlMultiSel {
		el["placeholder"] = plainObject("Select")
	}
	return []map[string]any{input(control, label, el)}
}

// options are the choices as options valued by index, or ok false if they cannot be.
func options(choices []string) (opts []map[string]any, ok bool) {
	if len(choices) > maxSelectOptions {
		return nil, false
	}
	for i, c := range choices {
		cleaned := chat.Clean(c, true)
		escaped := text(cleaned, 1<<30, true)
		if cleaned == "" || chat.Units(escaped) > maxOption {
			return nil, false
		}
		opts = append(opts, map[string]any{"text": plainObject(escaped), "value": strconv.Itoa(i)})
	}
	return opts, true
}

// numberedList is the question on a line of its own and then its choices, numbered, each in a field of its own,
// cut visibly if it is long. Every choice is in it.
func numberedList(label string, choices []string) []map[string]any {
	lines := make([]string, len(choices))
	for n, c := range choices {
		lines[n] = within(strconv.Itoa(n+1)+". ", cmp.Or(chat.Clean(c, true), "(blank)"), "", maxListLine, true)
	}
	return append([]map[string]any{section(label)}, fieldSections(lines)...)
}

// fieldSections is lines, which are escaped already and not empty, as the fields of sections, ten to a
// section. Each line is a field of its own, so that nothing a line says can run into the next.
func fieldSections(lines []string) []map[string]any {
	var out []map[string]any
	for len(lines) > 0 {
		n := min(len(lines), maxFields)
		fields := make([]map[string]any, n)
		for i, l := range lines[:n] {
			fields[i] = plainObject(l)
		}
		out = append(out, map[string]any{"type": "section", "fields": fields})
		lines = lines[n:]
	}
	return out
}

// userID is the shape of a Slack user ID, which is the only thing shown to a person as a mention.
var userID = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)

// mentionLine is a context line that names a user and says Text of them. The user is a mention only if it has the
// shape of a Slack ID, and then in an element of its own that holds nothing but the token; the words are a
// plain_text element, in which nothing is markup. Anything else is "someone".
func mentionLine(m chat.Mention) map[string]any {
	words := plainObject(text(m.Text, maxContext-40, true))
	if !userID.MatchString(m.User) {
		return map[string]any{"type": "context", "elements": []map[string]any{plainObject("someone"), words}}
	}
	return map[string]any{"type": "context", "elements": []map[string]any{mrkdwnObject("<@" + m.User + ">"), words}}
}

// renderEnding is e as a Slack message: how it ended, who by and when, what was asked, and for an answered
// prompt a line for each question and its answer, or, for a prompt of tools, each tool with its arguments and
// what was decided of it (toolEndingBlocks). No input or button is left. The only mrkdwn in it is the
// status line, which is built from a verified user ID, a time and a fixed word.
func renderEnding(e chat.Ending) (Content, error) {
	verb := map[chat.Outcome]string{chat.Answered: "Answered", chat.TimedOut: "Timed out", chat.Cancelled: "Cancelled"}[e.Outcome]
	status := "*" + verb + "*"
	if e.By != "" {
		who := "someone"
		if userID.MatchString(e.By) {
			who = "<@" + e.By + ">"
		}
		status += " by " + who
	}
	if !e.At.IsZero() {
		status += " " + dateToken(e.At)
	}
	blocks := []map[string]any{{"type": "section", "text": mrkdwnObject(status)}}
	var body []map[string]any
	switch {
	case len(e.Prompt.Tools) > 0: // the thread keeps what was asked, whether or not it was decided
		body = toolEndingBlocks(e)
	case e.Outcome == chat.Answered:
		body = answerSections(e.Prompt.Questions, e.Answers)
	}
	// The sub-agent's line and the hint are left out of an ending that has no room for them, rather than cutting what was decided.
	room := maxBlocks - len(blocks) - len(body)
	if e.Prompt.Subagent != "" && room > 0 {
		blocks, room = append(blocks, contextLine(subagentLine(e.Prompt.Subagent))), room-1
	}
	if hint := text(e.Prompt.Hint, maxHeader, true); hint != "" && room > 0 {
		blocks = append(blocks, contextLine(hint))
	}
	blocks = append(blocks, body...)
	if len(blocks) > maxBlocks {
		blocks = blocks[:maxBlocks]
	}
	return content(verb, blocks)
}

// answerSections is a field for each question and its answer, ten to a section. Each is one line: nothing a
// question or a choice says can start a line of its own.
func answerSections(qs []chat.Question, answers [][]string) []map[string]any {
	lines := make([]string, len(qs))
	for i, q := range qs {
		var given []string
		if i < len(answers) {
			given = answers[i]
		}
		question := cmp.Or(text(q.Text, maxQuestionInEnd, true), "Question "+strconv.Itoa(i+1))
		answer := "(no answer)"
		if len(given) > 0 {
			answer = cmp.Or(text(strings.Join(given, ", "), maxAnswerLine-chat.Units(question)-2, true), answer)
		}
		lines[i] = question + ": " + answer
	}
	return fieldSections(lines)
}
