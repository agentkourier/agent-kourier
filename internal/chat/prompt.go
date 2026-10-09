package chat

import (
	"fmt"
	"time"
)

// Prompt is a question an agent paused to ask: a hint, and one or more questions, each free text or a
// choice of options. It says nothing of how it looks: the adapter decides the controls (radio buttons or a
// menu, a numbered list for options that do not fit), the layout and the platform's limits. Every text
// field is literal.
type Prompt struct {
	// ID names the interaction. It is Agent Kourier's own, and comes back as Interaction.PromptID when the
	// prompt is answered.
	ID string `json:"id"`
	// Hint is the agent's one-line title for the question, or "".
	Hint string `json:"hint,omitempty"`
	// Questions is what is asked, in order.
	Questions []Question `json:"questions"`
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

// Validate says whether the prompt can be asked.
func (p Prompt) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("%w: a prompt with no ID", ErrInvalid)
	}
	if len(p.Questions) == 0 {
		return fmt.Errorf("%w: prompt %s has no question", ErrInvalid, p.ID)
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
}

// Validate says whether the ending can be shown.
func (e Ending) Validate() error {
	switch e.Outcome {
	case Answered, TimedOut, Cancelled:
	default:
		return fmt.Errorf("%w: an ending with the outcome %q", ErrInvalid, e.Outcome)
	}
	return nil
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
	// Answers is what the controls held when the button was pressed, one Answer for each question
	// that was answered. A question left unanswered has none.
	Answers []Answer
}

// Answer is the answer to question Question (an index into Prompt.Questions).
type Answer struct {
	Question int
	// Values is the text of a free text answer, or the index of each chosen option as a decimal string.
	Values []string
}
