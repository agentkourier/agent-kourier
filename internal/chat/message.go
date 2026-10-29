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

// Outgoing is what Agent Kourier says in a conversation: one of a Message, a Prompt (a question with controls)
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
		switch p := p.(type) {
		case Header, Body, Context, Note, Mention:
		case Steps:
			for j, st := range p.Steps {
				if st.ID == "" || !st.State.valid() {
					return fmt.Errorf("%w: step %d of part %d has no ID or a state of %d", ErrInvalid, j, i, st.State)
				}
			}
		case ToolLine:
			if !p.State.valid() {
				return fmt.Errorf("%w: part %d is a tool line of the state %q", ErrInvalid, i, p.State)
			}
		case Buttons:
			for j, btn := range p.Buttons {
				if btn.ID == "" || btn.Label == "" {
					return fmt.Errorf("%w: button %d of part %d has no ID or no label", ErrInvalid, j, i)
				}
			}
		default: // a nil part, or one of a type this package does not know: an adapter would drop it, silently
			return fmt.Errorf("%w: part %d is %T", ErrInvalid, i, p)
		}
	}
	return nil
}

// Part is one piece of a Message: Header, Body, Context, ToolLine, Note, Mention, Buttons or Steps. Every text field of
// a part is literal (see the package comment), except Body.Markdown.
type Part interface{ part() }

// Header is a title line: literal text, one line.
type Header struct{ Text string }

// Body is the text of a reply. Markdown is untrusted CommonMark, in which an adapter may allow emphasis,
// code and lists and must not allow a mention, a broadcast, a link, an image, HTML or a date.
type Body struct{ Markdown string }

// Context is a line of small print: literal text, one line.
type Context struct{ Text string }

// Note is a sentence Agent Kourier wrote itself, such as a notice: literal text, which may run to several lines.
type Note struct{ Text string }

// ToolState is how far the tool call of a ToolLine has got.
type ToolState string

// The states of a ToolLine. The zero value is ToolCalling, which is what a line stored before it had a state
// says, so a stored line without one is read as it always was.
const (
	// ToolCalling is a call under way.
	ToolCalling ToolState = ""
	// ToolRan is a call whose result has arrived and does not report a failure.
	ToolRan ToolState = "ran"
	// ToolFailed is a call whose result reports that it failed.
	ToolFailed ToolState = "failed"
)

func (s ToolState) valid() bool { return s == ToolCalling || s == ToolRan || s == ToolFailed }

// ToolLine is the status line of a tool call: the tool's name, a summary of its arguments, and how far the call has
// got. The caller redacts the secrets out of the arguments and cuts the summary before it makes this (a platform
// cannot: it sees one string), and the adapter shows name and summary as literal text on one line, in words of its own
// for the state: "Calling name(summary)", "Ran name(summary)" or "name(summary) failed".
type ToolLine struct {
	Name    string
	Summary string
	State   ToolState
}

// Mention is a line that names a person, and says something of them: User, as a mention where the platform has them,
// and then Text, literal text of Agent Kourier's own on one line, such as "answered this question first.". User is a
// platform's own user ID, which the adapter checks against the shape of the platform's IDs before it makes a
// mention of it; anything else is shown as "someone", and never as markup, whatever it holds.
type Mention struct{ User, Text string }

// Buttons is a row of buttons. A press comes back as an Interaction whose PromptID is the button's Value.
type Buttons struct{ Buttons []Button }

// Button is one button: Label is literal text on one line, and ID and Value are Agent Kourier's own, never text
// the agent wrote.
type Button struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
}

// Steps are the progress steps of a reply, such as its tool calls, each in the state to show, in order: a platform shows
// them as it shows a stream's steps, before the parts that follow. Each step is as Stream.Step takes it (Step), its title
// and detail literal text on one line, untrusted. It is how a message that replaces a stream keeps the steps the stream
// showed, which a platform such as Slack drops when it updates the message, and how a reply that is updated and never
// streams, such as an alert thread's, shows its tool calls. A platform without
// progress steps (Caps.Steps false) shows nothing of it. Only an update carries it today: no payload the outbox stores
// has one, so a release that cannot read it never meets one.
type Steps struct{ Steps []Step }

func (Header) part()   {}
func (Body) part()     {}
func (Context) part()  {}
func (Note) part()     {}
func (ToolLine) part() {}
func (Mention) part()  {}
func (Buttons) part()  {}
func (Steps) part()    {}

// wirePart is the stored form of a Part: its kind, and the fields of that kind.
type wirePart struct {
	Kind     string     `json:"kind"`
	Text     string     `json:"text,omitempty"`
	Markdown string     `json:"markdown,omitempty"`
	Name     string     `json:"name,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	State    string     `json:"state,omitempty"`
	User     string     `json:"user,omitempty"`
	Buttons  []Button   `json:"buttons,omitempty"`
	Steps    []wireStep `json:"steps,omitempty"`
}

// wireStep is the stored form of a Step: its state by name, and none for a step that is running.
type wireStep struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
	State  string `json:"state,omitempty"`
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
			w.Parts = append(w.Parts, wirePart{Kind: "tool", Name: p.Name, Summary: p.Summary, State: string(p.State)})
		case Mention:
			w.Parts = append(w.Parts, wirePart{Kind: "mention", User: p.User, Text: p.Text})
		case Buttons:
			w.Parts = append(w.Parts, wirePart{Kind: "buttons", Buttons: p.Buttons})
		case Steps:
			wp := wirePart{Kind: "steps"}
			for _, st := range p.Steps {
				ws := wireStep{ID: st.ID, Title: st.Title, Detail: st.Detail}
				if st.State != StepRunning {
					ws.State = st.State.String()
				}
				wp.Steps = append(wp.Steps, ws)
			}
			w.Parts = append(w.Parts, wp)
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
			out.Parts = append(out.Parts, ToolLine{Name: p.Name, Summary: p.Summary, State: ToolState(p.State)})
		case "mention":
			out.Parts = append(out.Parts, Mention{User: p.User, Text: p.Text})
		case "buttons":
			out.Parts = append(out.Parts, Buttons{p.Buttons})
		case "steps":
			var steps []Step
			for _, ws := range p.Steps {
				state, ok := stepStates[ws.State]
				if !ok {
					return fmt.Errorf("%w: a step of the state %q", ErrInvalid, ws.State)
				}
				steps = append(steps, Step{ID: ws.ID, Title: ws.Title, Detail: ws.Detail, State: state})
			}
			out.Parts = append(out.Parts, Steps{steps})
		default:
			return fmt.Errorf("%w: a part of kind %q", ErrInvalid, p.Kind)
		}
	}
	*m = out
	return nil
}

// stepStates are the states of a Step by their stored names: a running step is stored with none.
var stepStates = map[string]StepState{"": StepRunning, "done": StepDone, "failed": StepFailed, "ended": StepEnded}
