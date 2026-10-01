package render

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// Slack's limits, in characters. UNCONFIRMED(WP-04): documented, not measured.
const (
	maxPromptBlocks  = 50   // blocks in a message
	maxHeader        = 150  // a header block's text
	maxLabel         = 2000 // an input block's label
	maxSectionText   = 3000 // a section block's text
	maxOptionText    = 75   // an option's text, and a button's
	maxInlineOptions = 10   // options a radio-button group or a set of checkboxes holds
	maxSelectOptions = 100  // options a select holds
	maxListLine      = 200  // one choice in the numbered list a prompt falls back to
	maxQuestionLine  = 500  // a question's text at the head of that list
	defaultHeader    = "The agent needs your input"
	maxValue         = 2000 // a button's value
)

// ErrPromptTooLarge means the prompt does not fit in one message even in its fallback form: more
// than 50 blocks. It is not a rendering to retry: the caller reports the pause as unusable.
var ErrPromptTooLarge = errors.New("render: the prompt does not fit in one Slack message")

// The block and action IDs of the Submit button, which a press names.
const (
	SubmitBlockID  = "submit"
	SubmitActionID = "submit"
)

// ControlKind is what a question's control returns in state.values.
type ControlKind int

// The kinds of control.
const (
	// ControlText is a free-text input.
	ControlText ControlKind = iota
	// ControlChoice is one choice: the selected option's value is its index in Choices.
	ControlChoice
	// ControlChoices is any number of choices, each valued by index.
	ControlChoices
	// ControlNumbers is a free-text input beside a numbered list, for choices that cannot be
	// options: the text holds numbers, each a 1-based position in Choices.
	ControlNumbers
)

// Control says where in the press's state a question's answer is, and what it holds.
type Control struct {
	BlockID, ActionID string
	Kind              ControlKind
}

// PromptBlocks is the message of an ask-user pause: a header from the agent's hint, one input
// block per question, and a Submit button valued id, the stored interaction's. controls[i] is
// where question i's answer arrives. Every string the agent wrote goes through Text, so it is inert.
// Option values are indices into Question.Choices, never the choice text.
//
// A question whose choices cannot be options (more than 100, or one longer than 75 characters)
// is shown as a numbered list with a free-text input for the numbers: no choice is dropped.
func PromptBlocks(id, hint string, questions []dialect.Question) (blocks []map[string]any, controls []Control, err error) {
	if id == "" || len(questions) == 0 || units(id) > maxValue {
		return nil, nil, errors.New("render: a prompt needs an interaction ID and at least one question")
	}
	blocks = []map[string]any{{"type": "header", "text": plainText(cmp.Or(Text(hint, maxHeader), defaultHeader))}}
	for i, q := range questions {
		bs, c := questionBlocks(i, q)
		blocks, controls = append(blocks, bs...), append(controls, c)
	}
	blocks = append(blocks, map[string]any{"type": "actions", "block_id": SubmitBlockID, "elements": []map[string]any{
		{"type": "button", "action_id": SubmitActionID, "value": id, "style": "primary", "text": plainText("Submit")}}})
	if len(blocks) > maxPromptBlocks {
		return nil, nil, fmt.Errorf("%w: %d blocks", ErrPromptTooLarge, len(blocks))
	}
	return blocks, controls, nil
}

// questionBlocks is the blocks of question i, and its control.
func questionBlocks(i int, q dialect.Question) ([]map[string]any, Control) {
	c := Control{BlockID: "q" + strconv.Itoa(i), ActionID: "a" + strconv.Itoa(i)}
	label := cmp.Or(Text(q.Text, maxLabel), "Question "+strconv.Itoa(i+1))
	if len(q.Choices) == 0 {
		return layout(c, label, textInput("")), c
	}
	options, ok := options(q.Choices)
	if !ok {
		c.Kind = ControlNumbers
		input := layout(c, "Your answer: the number or numbers, separated by commas", textInput("For example: 2 or 1, 3"))
		return append(numberedList(q.Text, q.Choices), input...), c
	}
	c.Kind = ControlChoice
	kind := "radio_buttons"
	switch {
	case q.Multiple:
		c.Kind, kind = ControlChoices, "checkboxes"
		if len(options) > maxInlineOptions {
			kind = "multi_static_select"
		}
	case len(options) > maxInlineOptions:
		kind = "static_select"
	}
	element := map[string]any{"type": kind, "options": options}
	if strings.HasSuffix(kind, "select") {
		element["placeholder"] = plainText("Select")
	}
	return layout(c, label, element), c
}

// layout is the one place that decides how a question's control sits in the message: an input
// block, which holds its value in the press's state.values, with dispatch_action off so nothing is
// sent before Submit.
//
// UNCONFIRMED(WP-04): that input blocks work in a message, not only in a modal. If they do not,
// this is the only function to replace, with an actions block or a modal, and the controls it
// returns are unchanged. Slack does not enforce "optional": false in messages, so the completeness
// check of the answer is courier's.
func layout(c Control, label string, element map[string]any) []map[string]any {
	element["action_id"] = c.ActionID
	return []map[string]any{{"type": "input", "block_id": c.BlockID, "dispatch_action": false, "optional": false,
		"label": plainText(label), "element": element}}
}

func textInput(placeholder string) map[string]any {
	el := map[string]any{"type": "plain_text_input"}
	if placeholder != "" {
		el["placeholder"] = plainText(placeholder)
	}
	return el
}

func plainText(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s} }

// options are choices as options valued by index, or ok false if they cannot be: too many, or one
// that is empty or too long for an option's text once escaped.
func options(choices []string) (opts []map[string]any, ok bool) {
	if len(choices) > maxSelectOptions {
		return nil, false
	}
	for i, c := range choices {
		text := Text(c, math.MaxInt32)
		if strings.TrimSpace(text) == "" || units(text) > maxOptionText {
			return nil, false
		}
		opts = append(opts, map[string]any{"text": plainText(text), "value": strconv.Itoa(i)})
	}
	return opts, true
}

// numberedList is the question and its choices as numbered mrkdwn sections of at most 3000
// characters, each choice on a line of its own, cut visibly if it is long. Every choice is in it.
func numberedList(question string, choices []string) []map[string]any {
	var sections []map[string]any
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			sections = append(sections, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": b.String()}})
			b.Reset()
		}
	}
	b.WriteString(Text(question, maxQuestionLine) + "\n")
	for i, c := range choices {
		line := strconv.Itoa(i+1) + ". " + cmp.Or(strings.TrimSpace(Text(c, maxListLine)), "(blank)") + "\n"
		if units(b.String())+units(line) > maxSectionText {
			flush()
		}
		b.WriteString(line)
	}
	flush()
	return sections
}
