package chat

import (
	"encoding/json"
	"errors"
	"fmt"
)

// OutgoingVersion is the version of the stored form of an Outgoing. The outbox keeps an Outgoing as JSON
// and an adapter renders it at delivery, so a payload of another version is not one this code can read:
// the outbox abandons it as invalid.
const OutgoingVersion = 1

// Outgoing is what courier says in a conversation: one of a Message, a Prompt (a question with controls)
// or an Ending (a prompt's replacement). It is what the outbox stores, and what an adapter turns into its
// platform's own form at delivery. Exactly one of the three is set.
type Outgoing struct {
	V       int      `json:"v"`
	Message *Message `json:"message,omitempty"`
	Prompt  *Prompt  `json:"prompt,omitempty"`
	Ending  *Ending  `json:"ending,omitempty"`
}

// Say returns the Outgoing of a message.
func Say(m Message) Outgoing { return Outgoing{V: OutgoingVersion, Message: &m} }

// Ask returns the Outgoing of a prompt.
func Ask(p Prompt) Outgoing { return Outgoing{V: OutgoingVersion, Prompt: &p} }

// End returns the Outgoing of a prompt's replacement.
func End(e Ending) Outgoing { return Outgoing{V: OutgoingVersion, Ending: &e} }

// ErrInvalid wraps every error of a malformed Outgoing, which no retry mends.
var ErrInvalid = errors.New("chat: invalid message")

// Validate says whether o can be delivered: a known version, exactly one of its three forms, and the
// fields that form needs.
func (o Outgoing) Validate() error {
	if o.V != OutgoingVersion {
		return fmt.Errorf("%w: version %d, want %d", ErrInvalid, o.V, OutgoingVersion)
	}
	n := 0
	for _, set := range []bool{o.Message != nil, o.Prompt != nil, o.Ending != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%w: %d of message, prompt and ending are set, want 1", ErrInvalid, n)
	}
	switch {
	case o.Message != nil:
		return o.Message.Validate()
	case o.Prompt != nil:
		return o.Prompt.Validate()
	}
	return o.Ending.Validate()
}

// Message is plain content: a fallback text, and the parts that make it up. The fallback is what a
// notification or a reader that shows no parts displays, so it says everything on its own.
type Message struct {
	Fallback string
	Parts    []Part
}

// Validate says whether m says anything, and whether every part is complete.
func (m Message) Validate() error {
	if m.Fallback == "" && len(m.Parts) == 0 {
		return fmt.Errorf("%w: a message with no text and no parts", ErrInvalid)
	}
	for i, p := range m.Parts {
		if b, ok := p.(Buttons); ok {
			for j, btn := range b.Buttons {
				if btn.ID == "" || btn.Label == "" {
					return fmt.Errorf("%w: button %d of part %d has no ID or no label", ErrInvalid, j, i)
				}
			}
		}
	}
	return nil
}

// Part is one piece of a Message: Header, Body, Context, ToolLine, Note or Buttons. Every text field of
// a part is literal (see the package comment), except Body.Markdown.
type Part interface{ part() }

// Header is a title line: literal text, one line.
type Header struct{ Text string }

// Body is the text of a reply. Markdown is untrusted CommonMark, in which an adapter may allow emphasis,
// code and lists and must not allow a mention, a broadcast, a link, an image, HTML or a date.
type Body struct{ Markdown string }

// Context is a line of small print: literal text, one line.
type Context struct{ Text string }

// Note is a sentence courier wrote itself, such as a notice: literal text, which may run to several lines.
type Note struct{ Text string }

// ToolLine is the status line of a tool call: the tool's name and its arguments, which the caller has
// already redacted. The adapter shows them as literal text on one line.
type ToolLine struct {
	Name string
	Args map[string]string
}

// Buttons is a row of buttons. A press comes back as an Interaction whose PromptID is the button's Value.
type Buttons struct{ Buttons []Button }

// Button is one button: Label is literal text on one line, and ID and Value are courier's own, never text
// the agent wrote.
type Button struct {
	ID, Label, Value string
	Primary          bool
}

func (Header) part()   {}
func (Body) part()     {}
func (Context) part()  {}
func (Note) part()     {}
func (ToolLine) part() {}
func (Buttons) part()  {}

// wirePart is the stored form of a Part: its kind, and the fields of that kind.
type wirePart struct {
	Kind     string            `json:"kind"`
	Text     string            `json:"text,omitempty"`
	Markdown string            `json:"markdown,omitempty"`
	Name     string            `json:"name,omitempty"`
	Args     map[string]string `json:"args,omitempty"`
	Buttons  []Button          `json:"buttons,omitempty"`
}

type wireMessage struct {
	Fallback string     `json:"fallback,omitempty"`
	Parts    []wirePart `json:"parts,omitempty"`
}

// MarshalJSON stores m, with each part tagged by its kind.
func (m Message) MarshalJSON() ([]byte, error) {
	w := wireMessage{Fallback: m.Fallback}
	for _, p := range m.Parts {
		switch p := p.(type) {
		case Header:
			w.Parts = append(w.Parts, wirePart{Kind: "header", Text: p.Text})
		case Body:
			w.Parts = append(w.Parts, wirePart{Kind: "body", Markdown: p.Markdown})
		case Context:
			w.Parts = append(w.Parts, wirePart{Kind: "context", Text: p.Text})
		case Note:
			w.Parts = append(w.Parts, wirePart{Kind: "note", Text: p.Text})
		case ToolLine:
			w.Parts = append(w.Parts, wirePart{Kind: "tool", Name: p.Name, Args: p.Args})
		case Buttons:
			w.Parts = append(w.Parts, wirePart{Kind: "buttons", Buttons: p.Buttons})
		default:
			return nil, fmt.Errorf("%w: a part of type %T", ErrInvalid, p)
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads what MarshalJSON stored. A part of a kind it does not know is an error: a payload
// from a newer version is not guessed at.
func (m *Message) UnmarshalJSON(b []byte) error {
	var w wireMessage
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	out := Message{Fallback: w.Fallback}
	for _, p := range w.Parts {
		switch p.Kind {
		case "header":
			out.Parts = append(out.Parts, Header{p.Text})
		case "body":
			out.Parts = append(out.Parts, Body{p.Markdown})
		case "context":
			out.Parts = append(out.Parts, Context{p.Text})
		case "note":
			out.Parts = append(out.Parts, Note{p.Text})
		case "tool":
			out.Parts = append(out.Parts, ToolLine{Name: p.Name, Args: p.Args})
		case "buttons":
			out.Parts = append(out.Parts, Buttons{p.Buttons})
		default:
			return fmt.Errorf("%w: a part of kind %q", ErrInvalid, p.Kind)
		}
	}
	*m = out
	return nil
}
