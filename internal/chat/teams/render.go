package teams

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// maxMessageBytes is the most that one activity Agent Kourier sends holds, as JSON. Microsoft documents about 100 KB as the most a
// bot's message may hold (larger ones are refused with 413, MessageSizeTooBig; the limit is Microsoft's word, not measured), and
// the budget leaves the rest for what the Bot Connector adds. A message over it has its Body cut further, down to minBodyUnits.
const maxMessageBytes = 80 << 10

// maxTextUnits is the most UTF-16 units that one literal place of a message shows (Caps.MaxText), and maxBodyUnits what the Body
// does, its escapes and fences counted. Each is under the budget whatever the text holds: at most three bytes of JSON for each unit.
// minBodyUnits is the shortest that the Body is cut to when the whole message is over the budget.
const (
	maxTextUnits = 3000
	maxBodyUnits = 20000
	minBodyUnits = 1000
)

// mentionLabel is the name a mention shows: Agent Kourier does not read people's names on Teams yet, so a mention of anyone is
// "@someone", and notifies the person it names.
const mentionLabel = "someone"

// entraID is the shape of a Microsoft Entra object ID, a lowercase GUID: the user ID that a Mention or an Ending names, which is
// made a mention of no other shape.
var entraID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// outActivity is a message as the Bot Connector takes it: text in Teams' markdown, and the mentions in it.
type outActivity struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	TextFormat string          `json:"textFormat"`
	Entities   []mentionEntity `json:"entities,omitempty"`
}

// mentionEntity makes the text Text of an activity a mention of a person.
type mentionEntity struct {
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	Mentioned mentioned `json:"mentioned"`
}

type mentioned struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// capabilities is what a Teams connection does in this release: a message's steps, as a list under the rest of the message; and
// no streams, ephemeral messages, busy mark or reactions.
func capabilities() chat.Caps {
	return chat.Caps{Steps: true, MaxText: maxTextUnits, Limits: map[chat.Where]int{chat.WhereBody: maxBodyUnits, chat.WhereStream: maxBodyUnits}}
}

// errEmpty is wrapped by the error of an Outgoing that shows nothing on Teams (no part that shows text, and no Fallback).
var errEmpty = errors.New("the message shows nothing on Microsoft Teams")

// render is o as one activity within maxMessageBytes. Its error wraps chat.ErrInvalid for an Outgoing that is not valid, a prompt
// of tools or the ending of one (Teams shows no tool approvals yet), and one that shows nothing (errEmpty); and chat.ErrTooLarge
// for one over the budget even with its Body cut to minBodyUnits.
func render(o chat.Outgoing) (outActivity, error) {
	if err := o.Validate(); err != nil {
		return outActivity{}, err
	}
	switch {
	case o.Prompt != nil && len(o.Prompt.Tools) > 0, o.Ending != nil && len(o.Ending.Prompt.Tools) > 0:
		return outActivity{}, fmt.Errorf("%w: Microsoft Teams shows no tool approvals yet", chat.ErrInvalid)
	}
	for body := maxBodyUnits; ; body /= 2 {
		var r renderer
		r.caps, r.body = capabilities(), max(body, minBodyUnits)
		switch {
		case o.Message != nil:
			r.message(*o.Message)
		case o.Prompt != nil:
			r.prompt(*o.Prompt)
		default:
			r.ending(*o.Ending)
		}
		a := r.activity()
		if a.Text == "" {
			return outActivity{}, fmt.Errorf("%w: %w", chat.ErrInvalid, errEmpty)
		}
		n, err := size(a)
		if err != nil {
			return outActivity{}, err
		}
		if n <= maxMessageBytes {
			return a, nil
		}
		if !r.hasBody || body <= minBodyUnits {
			return outActivity{}, fmt.Errorf("%w: %d bytes, over the %d byte budget of a message", chat.ErrTooLarge, n, maxMessageBytes)
		}
	}
}

// size is the length of a as the connector sends it.
func size(a outActivity) (int, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return 0, fmt.Errorf("teams: encoding the activity: %w", err)
	}
	return buf.Len(), nil
}

// renderer builds the text of one activity: paragraphs, each of which is one line or one literal text, or the Body's markdown,
// with a blank line between them; and the mentions in them.
type renderer struct {
	caps     chat.Caps
	body     int // the limit of the Body for this try
	hasBody  bool
	paras    []string
	entities []mentionEntity
	fallback string
}

func (r *renderer) activity() outActivity {
	text := strings.Join(r.paras, "\n\n")
	if text == "" {
		text = r.fallback
	}
	return outActivity{Type: "message", Text: text, TextFormat: "markdown", Entities: r.entities}
}

// add appends a paragraph, unless it is empty.
func (r *renderer) add(p string) {
	if p != "" {
		r.paras = append(r.paras, p)
	}
}

// line adds one line: the pieces, each cleaned, joined, cut to the limit of w and escaped (lineOf). Every piece is literal: Agent
// Kourier's own words and the text of the place alike.
func (r *renderer) line(w chat.Where, pieces ...string) { r.add(r.lineOf(w, pieces...)) }

// lineOf is the pieces as one line: joined, cleaned, cut to the limit of w and escaped.
func (r *renderer) lineOf(w chat.Where, pieces ...string) string {
	return escapeLine(chat.Truncate(chat.Clean(strings.Join(pieces, ""), true), r.caps.Limit(w)))
}

// text adds the literal text s of several lines at w (textOf).
func (r *renderer) text(w chat.Where, s string) { r.add(r.textOf(w, s)) }

// textOf is the literal text s of several lines at w as one paragraph: each line without the white space at its ends, blank lines
// left out (a blank line would end the paragraph), cut to the limit of w, escaped.
func (r *renderer) textOf(w chat.Where, s string) string {
	var lines []string
	for _, l := range strings.Split(chat.Clean(s, false), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	cut := strings.Split(chat.Truncate(strings.Join(lines, "\n"), r.caps.Limit(w)), "\n")
	for i, l := range cut {
		cut[i] = escapeLine(l)
	}
	return strings.Join(cut, "\n")
}

// mentionLine adds a line that names user and then says the literal words, cut so that the line is within the limit of w. A user
// that is a Microsoft Entra object ID is a mention, "<at>someone</at>" with its entity; any other is the word "someone", as text.
func (r *renderer) mentionLine(w chat.Where, user, words string) {
	if !entraID.MatchString(user) {
		r.line(w, mentionLabel+" ", words)
		return
	}
	tag := "<at>" + mentionLabel + "</at>"
	r.entities = append(r.entities, mentionEntity{Type: "mention", Text: tag, Mentioned: mentioned{ID: user, Name: mentionLabel}})
	room := max(r.caps.Limit(w)-chat.Units(mentionLabel+" "), 0)
	r.add(strings.TrimSpace(tag + " " + escapeLine(chat.Truncate(chat.Clean(words, true), room))))
}

func (r *renderer) message(m chat.Message) {
	var lists []string // the step lists, which go under the rest of the message
	for i, p := range m.Parts {
		if _, heads := headOfSteps(m.Parts, i); heads {
			continue // said of the steps that follow it, and shown with them
		}
		switch p := p.(type) {
		case chat.Header:
			r.line(chat.WhereHeader, p.Text)
		case chat.Body:
			r.hasBody = true
			r.add(markdown(p.Markdown, r.body))
		case chat.Context:
			r.line(chat.WhereContext, p.Text)
		case chat.Note:
			r.text(chat.WhereNote, p.Text)
		case chat.ToolLine:
			name, summary := chat.Clean(p.Name, true), chat.Clean(p.Summary, true)
			switch p.State {
			case chat.ToolRan:
				r.line(chat.WhereToolName, "Ran ", name, "(", summary, ")")
			case chat.ToolFailed:
				r.line(chat.WhereToolName, "", name, "(", summary, ") failed")
			default:
				r.line(chat.WhereToolName, "Calling ", name, "(", summary, ")")
			}
		case chat.Mention:
			r.mentionLine(chat.WhereContext, p.User, p.Text)
		case chat.Buttons:
			// Teams shows no buttons yet: the labels are shown as text, so that what the message offers is still read.
			var labels []string
			for _, b := range p.Buttons {
				labels = append(labels, chat.Clean(b.Label, true))
			}
			r.line(chat.WhereButton, strings.Join(labels, " · "))
		case chat.Steps:
			if len(p.Steps) > 0 {
				head, _ := headOfSteps(m.Parts, i-1)
				lists = append(lists, r.stepList(head, p.Steps))
			}
		}
	}
	for _, l := range lists {
		r.add(l)
	}
	if len(r.paras) == 0 {
		r.fallback = r.textOf(chat.WhereFallback, m.Fallback)
	}
}

// headOfSteps says whether the part at i is a Context that heads the steps just after it (chat.Steps: such as how many earlier
// steps are not shown), and returns its text. A Steps part with no steps heads nothing, and the Context before it stays where it is.
func headOfSteps(parts []chat.Part, i int) (string, bool) {
	if i < 0 || i+1 >= len(parts) {
		return "", false
	}
	c, isContext := parts[i].(chat.Context)
	st, isSteps := parts[i+1].(chat.Steps)
	return c.Text, isContext && isSteps && len(st.Steps) > 0
}

// stepList is steps as one paragraph: a quote, which Teams shows with a bar beside it, of the line head, if there is one, and a
// list of the steps, one line each (stepLine).
func (r *renderer) stepList(head string, steps []chat.Step) string {
	var b strings.Builder
	if head != "" {
		b.WriteString("> " + r.lineOf(chat.WhereContext, head) + "\n>\n")
	}
	for i, st := range steps {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("> - " + r.stepLine(st))
	}
	return b.String()
}

// stepLine is a step as one line, cut to the limit of a step's title. A call is worded as the status line of a call is (ToolLine):
// "Calling name(arguments)" while it runs, "Ran name(arguments)" once done, "name(arguments) failed", and "name(arguments) ended
// with no result" for one whose result never came. A step that is no call, such as a card that stands for time, is its title
// and its detail after a "·", with "(failed)" or "(no result)" after them when it ended so.
func (r *renderer) stepLine(st chat.Step) string {
	title := cmp.Or(chat.Clean(st.Title, true), "(blank)")
	detail := chat.Clean(st.Detail, true)
	if st.Call {
		switch st.State {
		case chat.StepDone:
			return r.lineOf(chat.WhereStepTitle, "Ran ", title, "(", detail, ")")
		case chat.StepFailed:
			return r.lineOf(chat.WhereStepTitle, title, "(", detail, ") failed")
		case chat.StepEnded:
			return r.lineOf(chat.WhereStepTitle, title, "(", detail, ") ended with no result")
		}
		return r.lineOf(chat.WhereStepTitle, "Calling ", title, "(", detail, ")")
	}
	if detail != "" {
		title += " · " + detail
	}
	switch st.State {
	case chat.StepFailed:
		title += " (failed)"
	case chat.StepEnded:
		title += " (no result)"
	}
	return r.lineOf(chat.WhereStepTitle, title)
}

// noAnswers is the line under a prompt: a person cannot answer one on Teams yet.
const noAnswers = "This question cannot be answered in Microsoft Teams yet."

func (r *renderer) prompt(p chat.Prompt) {
	r.promptText(p)
	if !p.Deadline.IsZero() {
		r.line(chat.WhereFallback, "It closes at "+p.Deadline.UTC().Format(time.RFC1123)+".")
	}
	r.line(chat.WhereFallback, noAnswers)
}

// promptText adds what a prompt asked: its hint, its sub-agent, and each question with its options.
func (r *renderer) promptText(p chat.Prompt) {
	if p.Hint != "" {
		r.line(chat.WhereHint, p.Hint)
	}
	if p.Subagent != "" {
		r.line(chat.WhereSubagent, "Asked by the sub-agent ", p.Subagent)
	}
	for _, q := range p.Questions {
		r.line(chat.WhereQuestion, q.Text)
		for i, c := range q.Choices {
			r.line(chat.WhereChoice, "Option "+strconv.Itoa(i+1)+": ", c)
		}
	}
}

func (r *renderer) ending(e chat.Ending) {
	switch e.Outcome {
	case chat.Answered:
		if e.By != "" {
			r.mentionLine(chat.WhereUser, e.By, "answered.")
		} else {
			r.line(chat.WhereFallback, "Answered.")
		}
	case chat.TimedOut:
		r.line(chat.WhereFallback, "Nobody answered in time.")
	default:
		if e.By != "" {
			r.mentionLine(chat.WhereUser, e.By, "cancelled the question.")
		} else {
			r.line(chat.WhereFallback, "The question was cancelled.")
		}
	}
	p := e.Prompt
	if p.Hint != "" {
		r.line(chat.WhereHint, p.Hint)
	}
	if p.Subagent != "" {
		r.line(chat.WhereSubagent, "Asked by the sub-agent ", p.Subagent)
	}
	for i, q := range p.Questions {
		r.line(chat.WhereQuestion, q.Text)
		if i < len(e.Answers) && len(e.Answers[i]) > 0 {
			var answers []string
			for _, a := range e.Answers[i] {
				answers = append(answers, chat.Clean(a, true))
			}
			r.line(chat.WhereAnswer, "Answer: ", strings.Join(answers, ", "))
		}
	}
}

// escapeLine writes one line of literal text so that CommonMark shows its characters and acts on none of them: "&", "<", ">", "["
// and "]" as entities (so that no tag, mention, link or entity is made, whatever Teams does with a backslash), "{" and "}" as
// entities too (Teams dropped a "{…}" span of a step line in a live check, 2026-11-18; that the entities show as braces is
// unverified), the characters of emphasis, code and tables with a backslash, and a "#", "-", "+" or "=" at its start, or the "."
// or ")" after the digits it starts with, with a backslash, so that it is no heading, list item or underline.
//
// A bare URL is the one place where an escape may not be read: a reader that links bare URLs as they stand in the text (as the
// conformance suite's reading does) runs the link on over a backslash or an entity, and shows it as written. So a URL that holds a
// character that is escaped, or that a "<" or ">" ends, has its first letter written as a numeric entity, which no such reader takes
// for the start of a link: the URL is then text, whose escapes every reader reads. A reader that links URLs after it has read the
// escapes (GitHub's flavour does) links it as the address it shows, which a bare URL may be (chat's AllowAutolink).
func escapeLine(s string) string {
	s = strings.TrimLeft(s, " \t")
	var b strings.Builder
	digits := 0 // the digits the line starts with, while it has only digits so far
	for i := 0; i < len(s); i++ {
		c := s[i]
		start := i == 0
		if escapedURLAt(s, i) {
			b.WriteString("&#" + strconv.Itoa(int(c)) + ";")
			continue
		}
		switch c {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '[':
			b.WriteString("&#91;")
		case ']':
			b.WriteString("&#93;")
		case '{':
			b.WriteString("&#123;")
		case '}':
			b.WriteString("&#125;")
		case '\\', '`', '*', '_', '~', '|':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '#', '-', '+', '=':
			if start {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		case '.', ')':
			if digits == i && i > 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
		if c >= '0' && c <= '9' && digits == i {
			digits++
		}
	}
	return b.String()
}

// bareURL is a bare URL as a reader that links them finds one: http, https or ftp and "://", or "www.", then anything up to white
// space, "<" or ">".
var bareURL = regexp.MustCompile(`(?i)^(?:(?:https?|ftp)://|www\.)[^\s<>]+`)

// escapedURLAt says whether a bare URL starts at s[i], at the start of a word, and would not be shown as written if escapeLine
// escaped it: it holds a character that escapeLine escapes, or it ends at a "<" or ">", which escapeLine writes as an entity that
// does not end it.
func escapedURLAt(s string, i int) bool {
	if strings.IndexByte("hHfFwW", s[i]) < 0 {
		return false
	}
	if i > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:i]); unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	word := s[i:]
	if n := strings.IndexAny(word, " \t\n\f\r"); n >= 0 {
		word = word[:n]
	}
	url := bareURL.FindString(word)
	if url == "" {
		return false
	}
	if strings.ContainsAny(url, "&[]{}\\`*_~|") {
		return true
	}
	return len(url) < len(word) // it ends at a "<" or ">"
}
