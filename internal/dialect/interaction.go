package dialect

import "github.com/a2aproject/a2a-go/v2/a2a"

// Kind is what a paused agent asks a person to do.
type Kind string

// The kinds of interaction. A nested interaction is one of these, raised by a sub-agent
// (see Interaction.Nested), because a chat platform renders it as its inner kind.
const (
	// AskUser asks one or more questions, each with optional choices.
	AskUser Kind = "ask_user"
	// ToolApproval asks to approve or reject one or more tool calls.
	ToolApproval Kind = "tool_approval"
)

// Question is one question of an AskUser interaction.
type Question struct {
	Text     string
	Choices  []string // empty means free text
	Multiple bool     // more than one choice may be selected
}

// Tool is one tool call a ToolApproval interaction asks to approve.
type Tool struct {
	ID     string // what a decision names
	CallID string
	Name   string
	Args   map[string]any
}

// Nested says a sub-agent raised the interaction, and names the path to it.
type Nested struct {
	Subagent  string
	TaskID    string
	ContextID string
}

// Interaction is a pause the agent needs a person to answer. It holds what the dialect
// needs to build the answer, so keep it unchanged from the parse to the reply.
type Interaction struct {
	Kind      Kind
	TaskID    a2a.TaskID // the paused task, which the answer is addressed to
	ContextID string
	RequestID string     // AskUser: the id the answer must echo
	Questions []Question // AskUser
	Tools     []Tool     // ToolApproval
	Nested    *Nested    // nil unless a sub-agent raised it
}

// Decision is a person's verdict on one tool call.
type Decision struct {
	ToolID   string // Tool.ID
	Approved bool
	Reason   string // why it was rejected; ignored when approved
}

// Answer is a person's reply to an Interaction.
type Answer struct {
	// RequestID echoes Interaction.RequestID, so a reply to an older pause is refused.
	RequestID string
	// Answers holds, for each question in order, the chosen choices or the free text.
	Answers [][]string
	// Decisions holds one verdict for each tool of a ToolApproval.
	Decisions []Decision
}
