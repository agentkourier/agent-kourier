package teams

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// This file is the Adaptive Card layout of the neutral model: a prompt, the ending that replaces it, and a message's buttons. A
// button is an Action.Execute, which Teams sends back as an adaptiveCard/action invoke (press.go); Universal Actions need a card of
// version 1.5 (Microsoft's "Work with Universal Actions for Adaptive Cards", updated 2026-07-27). The cards this file renders were
// posted, pressed and replaced on a test tenant, web client, channel thread, November 2026:
//   - a TextRun and a choice's title are literal: "<at>Someone</at> **bold** {braces} [link](url)" showed exactly as written, with no
//     mention, markdown or link formed, and a monospace run kept its braces and its line breaks;
//   - a RichTextBlock, a multi-line Input.Text, radio buttons, checkboxes for a choice of several and a menu past inlineChoices all
//     showed, and so did the positive and destructive styles and several actions;
//   - a press came as one adaptiveCard/action invoke with the verb actionVerb, naming the card's message in its replyToId, with the
//     inputs merged into the data by their IDs and a choice of several joined with commas ("0,1");
//   - the answer that press.go gives showed the presser a banner under the card, and the card's inputs were reset;
//   - an Update with the ending replaced the card, and Teams marked the message "Edited";
//   - a message of buttons alone showed its text (the message's Fallback) above the card.
//
// Every text that the agent, a person or an alert wrote is the text of a TextRun in a RichTextBlock, or the title of a choice, which
// shows the characters it holds. So the text is cleaned, put on one line where its place is one, and cut at its place's limit, and is
// not escaped: an escape would be shown. The one TextBlock is the line that names who decided, which holds only Agent Kourier's own
// words and the "<at>" of a mention, with its entity in the card's msteams entities. The labels of the inputs are not set, and an
// action's title is a button's label, Agent Kourier's own.

// The card's fixed parts.
const (
	// cardContentType is the content type of an Adaptive Card attachment.
	cardContentType = "application/vnd.microsoft.card.adaptive"
	cardSchema      = "http://adaptivecards.io/schemas/adaptive-card.json"
	cardVersion     = "1.5"

	// dataKey is the key of Agent Kourier's own part of an action's data. Teams merges the values of the card's inputs into the data
	// by their IDs (the Adaptive Cards schema of Action.Execute), and no input's ID is this.
	dataKey = "kourier"
	// actionVerb is the verb of every Action.Execute of a card: Universal Actions route an action by its verb, and the press is read
	// by its data, so the verb is one fixed word.
	actionVerb = "kourier"

	// inlineChoices is the most choices a question shows as radio buttons or checkboxes; one with more shows them in a menu.
	inlineChoices = 10
	// minEndingUnits is the shortest that an ending's texts are cut to, to fit the budget of a message: an ending cannot be refused,
	// or its prompt would keep its controls. Twenty tools, or twenty questions with their answers, each of texts this long of the
	// characters that cost the most bytes, fit.
	minEndingUnits = 200
)

// The fixed texts of a card. None holds anything the agent wrote.
const (
	inputHeader    = "The agent needs your input"
	inputFallback  = "The agent needs your input."
	approvalHeader = "The agent needs your approval"
	approvalFall   = "The agent needs your approval."
	noArguments    = "(no arguments)"
	blankChoice    = "(blank)"
)

// attachment is one attachment of an activity: here, always an Adaptive Card.
type attachment struct {
	ContentType string         `json:"contentType"`
	Content     map[string]any `json:"content"`
}

// textStyle is how a TextRun looks.
type textStyle struct{ bold, subtle, mono bool }

var (
	plainText  = textStyle{}
	boldText   = textStyle{bold: true}
	subtleText = textStyle{subtle: true}
	monoText   = textStyle{mono: true}
)

// cardBuilder builds the body of one card and the mentions in it.
type cardBuilder struct {
	caps chat.Caps
	// cut, when not 0, is the most units of any text of the card, below its place's limit: an ending cut to fit the budget.
	cut int
	// decider is the name of an ending's decider as the roster gives it, or "" (mentionLabel).
	decider  string
	body     []any
	entities []any
}

// limit is the most units of text at w on this card.
func (b *cardBuilder) limit(w chat.Where) int {
	n := b.caps.Limit(w)
	if b.cut > 0 {
		n = min(n, b.cut)
	}
	return n
}

// literal is s as the text at w shows it: cleaned, on one line for a place of one, and cut to the limit.
func (b *cardBuilder) literal(w chat.Where, s string) string {
	return chat.Truncate(chat.Clean(s, w.Line()), b.limit(w))
}

// text adds s as a RichTextBlock of one TextRun in the style st, unless it is empty.
func (b *cardBuilder) text(st textStyle, s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	run := map[string]any{"type": "TextRun", "text": s}
	if st.bold {
		run["weight"] = "Bolder"
	}
	if st.subtle {
		run["isSubtle"] = true
	}
	if st.mono {
		run["fontType"] = "Monospace"
	}
	b.body = append(b.body, map[string]any{"type": "RichTextBlock", "inlines": []any{run}})
}

// card is the card of the body built, with the actions and the fallbackText, Agent Kourier's own words, if there are any.
func (b *cardBuilder) card(fallback string, actions []any) map[string]any {
	c := map[string]any{"type": "AdaptiveCard", "$schema": cardSchema, "version": cardVersion}
	if fallback != "" {
		c["fallbackText"] = fallback
	}
	if len(b.body) > 0 {
		c["body"] = b.body
	}
	if len(actions) > 0 {
		c["actions"] = actions
	}
	if len(b.entities) > 0 {
		c["msteams"] = map[string]any{"entities": b.entities}
	}
	return c
}

// execute is an Action.Execute with the title, the style ("" for none) and Agent Kourier's data.
func execute(title, style string, data map[string]any) map[string]any {
	a := map[string]any{"type": "Action.Execute", "verb": actionVerb, "title": title, "data": map[string]any{dataKey: data}}
	if style != "" {
		a["style"] = style
	}
	return a
}

// promptAction is the action of a prompt's button: the prompt's ID and which button it is.
func promptAction(title, style, prompt string, act chat.Action) map[string]any {
	return execute(title, style, map[string]any{"prompt": prompt, "action": act.String()})
}

// The IDs of a prompt's inputs, which come back in a press's data: the input of question i (or of tool i of several) is a letter that
// says what it is and then i, so that an answer is read as the control it was asked in (press.go).
const (
	inputText   = 't' // a free-text input
	inputSingle = 'r' // a choice of one: radio buttons or a menu
	inputMulti  = 'c' // a choice of several: checkboxes or a menu
)

func inputID(control byte, i int) string { return string(rune(control)) + strconv.Itoa(i) }

// choiceSet is the input of question i that offers titles, valued by index: shown inline up to inlineChoices, and in a menu past it.
func choiceSet(i int, multiple bool, titles []string) map[string]any {
	control, style := byte(inputSingle), "expanded"
	if multiple {
		control = inputMulti
	}
	if len(titles) > inlineChoices {
		style = "compact"
	}
	choices := make([]any, len(titles))
	for j, t := range titles {
		choices[j] = map[string]any{"title": t, "value": strconv.Itoa(j)}
	}
	return map[string]any{"type": "Input.ChoiceSet", "id": inputID(control, i), "style": style, "isMultiSelect": multiple, "choices": choices}
}

// header adds a prompt's hint, or the fixed words in its place when it has none, and the line of the sub-agent that asked.
func (b *cardBuilder) header(p chat.Prompt, fixed string) {
	b.text(boldText, cmp.Or(b.literal(chat.WhereHint, p.Hint), fixed))
	b.subagent(p.Subagent)
}

func (b *cardBuilder) subagent(name string) {
	if name != "" {
		b.text(subtleText, b.literal(chat.WhereSubagent, "Asked by the sub-agent "+name))
	}
}

// deadline adds when a prompt closes, if it does.
func (b *cardBuilder) deadline(at time.Time) {
	if !at.IsZero() {
		b.text(subtleText, "It closes at "+at.UTC().Format(time.RFC1123)+".")
	}
}

// question adds the text of question i, or "Question N" when it has none.
func (b *cardBuilder) question(i int, q chat.Question) {
	b.text(boldText, cmp.Or(b.literal(chat.WhereQuestion, q.Text), "Question "+strconv.Itoa(i+1)))
}

// prompt is the card of p: its header, each question with its control, when it closes, and Submit; or, for a prompt of tools,
// toolPrompt's. Its error wraps chat.ErrTooLarge for a tool that is shown cut.
func (b *cardBuilder) prompt(p chat.Prompt) (map[string]any, error) {
	if len(p.Tools) > 0 {
		return b.toolPrompt(p)
	}
	b.header(p, inputHeader)
	for i, q := range p.Questions {
		b.question(i, q)
		if len(q.Choices) == 0 {
			b.body = append(b.body, map[string]any{"type": "Input.Text", "id": inputID(inputText, i), "isMultiline": true})
			continue
		}
		titles := make([]string, len(q.Choices))
		for j, c := range q.Choices {
			titles[j] = cmp.Or(b.literal(chat.WhereChoice, c), blankChoice)
		}
		b.body = append(b.body, choiceSet(i, q.Multiple, titles))
	}
	b.deadline(p.Deadline)
	return b.card(inputFallback, []any{promptAction("Submit", "positive", p.ID, chat.ActionSubmit)}), nil
}

// shownName is the name of tool i of n as the person is told it: cleaned, on one line, and numbered if there are several tools ("2.
// name"); or "Tool N" if there is nothing left of the name. The number counts toward the name's limit, which is the whole of what is
// shown.
func shownName(name string, i, n int) string {
	cleaned := chat.Clean(name, true)
	switch {
	case cleaned == "":
		return "Tool " + strconv.Itoa(i+1)
	case n > 1:
		return strconv.Itoa(i+1) + ". " + cleaned
	}
	return cleaned
}

// args adds the cleaned arguments of a tool, in monospace, or the words that say it has none.
func (b *cardBuilder) args(cleaned string) {
	if strings.TrimSpace(cleaned) == "" {
		b.text(subtleText, noArguments)
		return
	}
	b.text(monoText, cleaned)
}

// toolPrompt is the card of a prompt of tools. One tool is its name and arguments, and Approve and Deny, which decide it. Several are
// each a numbered name, the arguments and, just under them, a choice of Approve ("0", chat.ToolApproveValue) or Deny ("1",
// chat.ToolDenyValue), so that no choice reads as another tool's; then Submit, Approve all and Deny all. A tool whose cleaned name or
// arguments are over their place's limit is chat.ErrTooLarge, and never shown cut, since a person cannot approve what they cannot read.
func (b *cardBuilder) toolPrompt(p chat.Prompt) (map[string]any, error) {
	b.header(p, approvalHeader)
	for i, t := range p.Tools {
		name, args := shownName(t.Name, i, len(p.Tools)), chat.Clean(t.Args, false)
		if n, limit := chat.Units(name), b.limit(chat.WhereToolName); n > limit {
			return nil, fmt.Errorf("%w: the name of tool %d is %d units, at most %d", chat.ErrTooLarge, i+1, n, limit)
		}
		if n, limit := chat.Units(args), b.limit(chat.WhereToolArgs); n > limit {
			return nil, fmt.Errorf("%w: the arguments of tool %d are %d units, at most %d", chat.ErrTooLarge, i+1, n, limit)
		}
		b.text(boldText, name)
		b.args(args)
		if len(p.Tools) > 1 {
			b.body = append(b.body, choiceSet(i, false, []string{"Approve", "Deny"}))
		}
	}
	b.deadline(p.Deadline)
	actions := []any{promptAction("Approve", "positive", p.ID, chat.ActionApproveAll), promptAction("Deny", "destructive", p.ID, chat.ActionDenyAll)}
	if len(p.Tools) > 1 {
		actions = []any{
			promptAction("Submit", "positive", p.ID, chat.ActionSubmit),
			promptAction("Approve all", "", p.ID, chat.ActionApproveAll),
			promptAction("Deny all", "destructive", p.ID, chat.ActionDenyAll),
		}
	}
	return b.card(approvalFall, actions), nil
}

// who adds the line that says who decided: a mention of by, beside the words, when it is a Microsoft Entra object ID; "someone" and the
// words, as text, for any other; and alone, the line to say when no one is named. The mention works in a card (styled, and the person
// notified, on a test tenant, web client, channel thread, November 2026), and Teams shows the entity's own text and puts in no name, so
// the tag and the entity hold the decider's name (b.decider, cleaned by mentionName), or mentionLabel with none. The name is read by
// Update from the roster (mentionNames): the invoke's from.name is not carried, since the press reaches the ending as chat.Interaction
// and the interaction's Decider, which hold the user's ID alone. That the tag shows a name as written was not measured.
func (b *cardBuilder) who(by, words, alone string) {
	switch {
	case entraID.MatchString(by):
		name := cmp.Or(mentionName(b.decider), mentionLabel)
		tag := "<at>" + name + "</at>"
		b.entities = append(b.entities, map[string]any{"type": "mention", "text": tag, "mentioned": map[string]any{"id": by, "name": name}})
		b.body = append(b.body, map[string]any{"type": "TextBlock", "text": tag + " " + words, "wrap": true})
	case by != "":
		b.text(plainText, mentionLabel+" "+words)
	default:
		b.text(plainText, alone)
	}
}

// ending is the card that replaces a prompt once it has ended: how it ended and who decided, what was asked, and for each question
// its answer, or for each tool one line, as a status line shows a call: its name (numbered if there are several), the summary of its
// arguments (chat.Tool.Summary) in parentheses and what was decided of it, as in "2. kubectl(ns="prod") — Denied", or with no
// arguments for a tool whose call the thread shows on its card already (chat.Tool.OnCard), as in "2. kubectl — Denied". The arguments
// whole are the prompt's, which the person read before deciding. It has no controls, and its fallbackText is the outcome.
func (b *cardBuilder) ending(e chat.Ending) map[string]any {
	verb := "Answered"
	switch e.Outcome {
	case chat.Answered:
		b.who(e.By, "answered.", "Answered.")
	case chat.TimedOut:
		verb = "Timed out"
		b.text(plainText, "Nobody answered in time.")
	default:
		verb = "Cancelled"
		b.who(e.By, "cancelled the question.", "The question was cancelled.")
	}
	p := e.Prompt
	if p.Hint != "" {
		b.text(subtleText, b.literal(chat.WhereHint, p.Hint))
	}
	b.subagent(p.Subagent)
	for i := range p.Tools {
		b.text(plainText, b.endedTool(e, i))
	}
	for i, q := range p.Questions {
		b.question(i, q)
		if i < len(e.Answers) && len(e.Answers[i]) > 0 {
			var answers []string
			for _, a := range e.Answers[i] {
				answers = append(answers, chat.Clean(a, true))
			}
			b.text(plainText, b.literal(chat.WhereAnswer, "Answer: "+strings.Join(answers, ", ")))
		}
	}
	return b.card(verb+".", nil)
}

// endedTool is the line of tool i of the ended prompt e (ending), within the cap of a line that holds a tool's name and its
// arguments: the summary cut to half of it at most, the verdict whole, and the name to what is left, each cut marked. A tool whose
// call the thread shows on its card (chat.Tool.OnCard) has no summary, and its name the cap less the verdict.
func (b *cardBuilder) endedTool(e chat.Ending, i int) string {
	tools := e.Prompt.Tools
	limit := min(b.limit(chat.WhereToolName), b.limit(chat.WhereToolArg))
	rest := ""
	if !tools[i].OnCard {
		rest = "(" + chat.Truncate(tools[i].Summary(), limit/2) + ")"
	}
	if i < len(e.Verdicts) {
		rest += " — " + map[chat.ToolVerdict]string{chat.ToolApproved: "Approved", chat.ToolDenied: "Denied"}[e.Verdicts[i]]
	}
	return chat.Truncate(shownName(tools[i].Name, i, len(tools)), limit-chat.Units(rest)) + rest
}

// buttonsCard is the card of a message's buttons, in order: a card of actions alone, posted under the message's text. Each is named by
// its ID and value, which are Agent Kourier's own, and, when of is set, by the text message's ID ("of") and mac of the button's value
// ("mac"), which bind the press to that text (press); a label cut to nothing reads "(blank)". It is nil for no buttons.
func buttonsCard(caps chat.Caps, buttons []chat.Button, of string, mac func(value string) string) map[string]any {
	if len(buttons) == 0 {
		return nil
	}
	b := cardBuilder{caps: caps}
	actions := make([]any, len(buttons))
	for i, btn := range buttons {
		style := ""
		if btn.Primary {
			style = "positive"
		}
		data := map[string]any{"button": btn.ID, "value": btn.Value}
		if of != "" {
			data["of"], data["mac"] = of, mac(btn.Value)
		}
		actions[i] = execute(cmp.Or(b.literal(chat.WhereButton, btn.Label), blankChoice), style, data)
	}
	return b.card("", actions)
}
