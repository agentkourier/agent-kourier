package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// Prompt is what an agent paused to ask: a hint, and either one or more questions, each free text or a
// choice of options, or the tool calls it wants approved. A prompt holds questions or tools, never both and
// never neither (Validate). It says nothing of how it looks: the adapter decides the controls (radio buttons
// or a menu, a numbered list for options that do not fit), the layout and the platform's limits. Every text
// field is literal.
type Prompt struct {
	// ID names the interaction. It is Agent Kourier's own, and comes back as Interaction.PromptID when the
	// prompt is answered.
	ID string `json:"id"`
	// Hint is the agent's one-line title for the question, or "".
	Hint string `json:"hint,omitempty"`
	// Questions is what is asked, in order. It is empty in a prompt of tools.
	Questions []Question `json:"questions,omitempty"`
	// Tools are the tool calls awaiting approval, in order. It is empty in a prompt of questions.
	Tools []Tool `json:"tools,omitempty"`
	// Deadline is when the question closes, or the zero time if it does not.
	Deadline time.Time `json:"deadline,omitzero"`
	// Subagent names the sub-agent that raised a nested pause, or "".
	Subagent string `json:"subagent,omitempty"`
}

// Question is one question of a Prompt. With no Choices it is free text.
type Question struct {
	Text string `json:"text"`
	// Choices are the options, in order. An answer to a choice question is the index of each option the
	// person picked, as a decimal string, never the option's text.
	Choices []string `json:"choices,omitempty"`
	// Multiple lets more than one choice be picked.
	Multiple bool `json:"multiple,omitempty"`
}

// Tool is one tool call of a Prompt awaiting approval. Both fields are literal text that the agent wrote
// and nobody has checked: the model carries them with no markup, and the adapter makes them inert. Redaction
// is the caller's: Args arrives already redacted and serialised. An adapter shows both cleaned and whole: a
// prompt with a tool that does not fit its place's limit is ErrTooLarge (Platform.Fits) and is never shown
// cut, because a person cannot approve what they cannot read. The ending of a prompt, which cannot be
// refused, shows each tool on one line, its arguments as their Summary, and is cut and marked like any other text.
type Tool struct {
	// Name is the tool's name. It may be empty, or clean to nothing; the adapter then shows a placeholder, so
	// that the tool can still be told apart and decided.
	Name string `json:"name"`
	// Args are the tool's arguments as text, in the caller's own serialisation, or "" if it has none. They
	// may run to several lines.
	Args string `json:"args,omitempty"`
}

// MaxToolSummary is the most UTF-16 units of a tool's arguments that the ending of a prompt of tools shows (Tool.Summary): a
// line, as long as the summary of a call in a status line.
const MaxToolSummary = 160

// Summary is the tool's arguments on one line, as the ending of its prompt shows them, and as a status line shows a call's: a
// JSON object, which is what Agent Kourier makes of a tool call's arguments, as "key=value, ..." in the order of the keys, each
// value compact JSON as written; any other text as it is. It is cleaned onto one line and cut to MaxToolSummary units, the
// cut marked with Truncated, and is "" for a tool with no arguments. It shows nothing of the arguments that Args does not, so it
// holds no secret that Args, which arrives redacted, does not, and it is not redacted again. It is literal: the adapter makes it
// inert.
func (t Tool) Summary() string {
	s := t.Args
	if pairs, ok := objectPairs(t.Args); ok {
		s = pairs
	}
	return Truncate(Clean(s, true), MaxToolSummary)
}

// objectPairs is the JSON object s as "key=value, ...", sorted by key, each value compact JSON with "<", ">" and "&" as they
// are and a number as it was written; ok is false if s is not one JSON object and nothing more.
func objectPairs(s string) (pairs string, ok bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil || obj == nil {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	keys := slices.Sorted(maps.Keys(obj))
	parts := make([]string, len(keys))
	for i, k := range keys {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(obj[k]) != nil {
			return "", false
		}
		parts[i] = k + "=" + strings.TrimSuffix(b.String(), "\n")
	}
	return strings.Join(parts, ", "), true
}

// Validate says whether the prompt can be asked: it has an ID, and either questions or tools, not both.
func (p Prompt) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("%w: a prompt with no ID", ErrInvalid)
	}
	switch {
	case len(p.Questions) > 0 && len(p.Tools) > 0:
		return fmt.Errorf("%w: prompt %s has both questions and tools", ErrInvalid, p.ID)
	case len(p.Questions) == 0 && len(p.Tools) == 0:
		return fmt.Errorf("%w: prompt %s has no question and no tool", ErrInvalid, p.ID)
	}
	return nil
}

// Outcome is how a prompt ended.
type Outcome string

// The outcomes of a prompt.
const (
	Answered  Outcome = "answered"  // a person submitted a complete answer
	TimedOut  Outcome = "timed_out" // nobody answered by the deadline
	Cancelled Outcome = "cancelled" // the task was cancelled, by Stop or because it ended some other way
)

// Ending is what replaces a prompt once it has ended: who decided and when, and a summary of the answer,
// with no controls left.
type Ending struct {
	Outcome Outcome `json:"outcome"`
	// By is the user who decided (Answered) or stopped it (Cancelled), as Agent Kourier verified it, or "". It
	// is a user ID and not text: the adapter shows it as a mention only if the platform's own ID has
	// that shape, and otherwise as "someone".
	By string `json:"by,omitempty"`
	// At is when it ended, or the zero time if that is not known.
	At time.Time `json:"at,omitzero"`
	// Prompt is what was asked.
	Prompt Prompt `json:"prompt"`
	// Answers is, for Answered, what to show of each question's answer, in order, each on one line: the
	// text of each chosen option, and for a free-text answer a fixed word of Agent Kourier's, never what the person
	// typed, which may be a secret and which the channel would keep (the owner's rule). An adapter shows what
	// it is given.
	Answers [][]string `json:"answers,omitempty"`
	// Verdicts is, for an Answered prompt of tools, what each tool was decided, in the order of Prompt.Tools.
	// A TimedOut or Cancelled ending needs none.
	Verdicts []ToolVerdict `json:"verdicts,omitempty"`
}

// ToolVerdict is what a person decided for one tool call.
type ToolVerdict string

// The verdicts on a tool call.
const (
	ToolApproved ToolVerdict = "approved"
	ToolDenied   ToolVerdict = "denied"
)

// Validate says whether the ending can be shown. An Answered ending of a prompt of tools has one known
// verdict for each tool, and no ending has verdicts that do not match its tools.
func (e Ending) Validate() error {
	switch e.Outcome {
	case Answered, TimedOut, Cancelled:
	default:
		return fmt.Errorf("%w: an ending with the outcome %q", ErrInvalid, e.Outcome)
	}
	if len(e.Verdicts) > 0 || (e.Outcome == Answered && len(e.Prompt.Tools) > 0) {
		if len(e.Verdicts) != len(e.Prompt.Tools) {
			return fmt.Errorf("%w: an ending with %d verdicts for %d tools", ErrInvalid, len(e.Verdicts), len(e.Prompt.Tools))
		}
		for i, v := range e.Verdicts {
			if v != ToolApproved && v != ToolDenied {
				return fmt.Errorf("%w: the verdict %q of tool %d", ErrInvalid, v, i)
			}
		}
	}
	return nil
}

// Action is which of a prompt's buttons was pressed. The zero value is Submit, so a press that names no
// action, such as one recorded before actions existed, is a submit.
type Action int

// The buttons of a prompt. Submit is the one button of a prompt of questions, and the button that sends the
// per-tool choices of a prompt of several tools. ApproveAll and DenyAll decide every tool at once, and
// ignore whatever the controls held.
const (
	ActionSubmit Action = iota
	ActionApproveAll
	ActionDenyAll
)

var actionNames = [...]string{ActionSubmit: "submit", ActionApproveAll: "approve_all", ActionDenyAll: "deny_all"}

// String names the action, for a log or a stored press.
func (a Action) String() string {
	if a < 0 || int(a) >= len(actionNames) {
		return "unknown"
	}
	return actionNames[a]
}

// MarshalText stores the action by name, so that a number is never renumbered under a stored press. An
// action Agent Kourier does not define is ErrInvalid.
func (a Action) MarshalText() ([]byte, error) {
	if a < 0 || int(a) >= len(actionNames) {
		return nil, fmt.Errorf("%w: an action numbered %d", ErrInvalid, int(a))
	}
	return []byte(actionNames[a]), nil
}

// UnmarshalText reads an action stored by MarshalText. A name it does not know is ErrInvalid.
func (a *Action) UnmarshalText(text []byte) error {
	for i, name := range actionNames {
		if string(text) == name {
			*a = Action(i)
			return nil
		}
	}
	return fmt.Errorf("%w: the action %q", ErrInvalid, text)
}

// Interaction is a person pressing a prompt's button: the submit of an ask-user prompt, say. The adapter
// decodes the platform's own state of the prompt's controls into Answers, so that no caller sees a
// platform's element types or IDs.
type Interaction struct {
	// User is who pressed, as the platform names them.
	User string
	// At is the prompt's message.
	At Ref
	// PromptID is the pressed button's Value: the Prompt.ID of the interaction it answers.
	PromptID string
	// Action is which button was pressed. The zero value is ActionSubmit.
	Action Action
	// Answers is what the controls held when the button was pressed, one Answer for each question
	// that was answered. A question left unanswered has none. For a prompt of tools it holds one Answer for
	// each tool that has a verdict chosen, with Question the index into Prompt.Tools and Values one of
	// ToolApproveValue and ToolDenyValue (see Answer.ToolVerdict); a tool left undecided has none. It is
	// empty for ActionApproveAll and ActionDenyAll, which ignore it.
	Answers []Answer
	// Delivery is an opaque key of this press that the platform sends again with the press when it delivers it again (a
	// retry of a request whose answer was lost, say), and that another press, by the same person on the same button
	// included, does not carry. It is for recording a press once, and nothing reads what it holds. Empty: the platform
	// gave nothing to key the press by.
	Delivery string
}

// Answer is the answer to question Question (an index into Prompt.Questions).
type Answer struct {
	Question int
	// Values is the text of a free text answer, or the index of each chosen option as a decimal string.
	Values []string
}

// The values of an Answer to a tool of a prompt of tools: the index of the option chosen, as a decimal
// string, as for a choice question, in the fixed order approve, deny.
const (
	ToolApproveValue = "0"
	ToolDenyValue    = "1"
)

// ToolVerdict reads the answer to a tool of a prompt of tools: exactly one value, ToolApproveValue or
// ToolDenyValue. Anything else, an answer with no value included, is not a verdict.
func (a Answer) ToolVerdict() (ToolVerdict, bool) {
	if len(a.Values) != 1 {
		return "", false
	}
	switch a.Values[0] {
	case ToolApproveValue:
		return ToolApproved, true
	case ToolDenyValue:
		return ToolDenied, true
	}
	return "", false
}
