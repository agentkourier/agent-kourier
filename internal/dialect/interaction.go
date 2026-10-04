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
	Text     string   `json:"text"`
	Choices  []string `json:"choices"`  // empty (nil or zero-length) means free text; test with len
	Multiple bool     `json:"multiple"` // more than one choice may be selected
}

// Tool is one tool call a ToolApproval interaction asks to approve.
type Tool struct {
	ID     string         `json:"id"` // what a decision names
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// ToolPartKind says whether an artifact part reports a tool call or its result.
type ToolPartKind string

// The kinds of tool part.
const (
	ToolCall   ToolPartKind = "call"
	ToolResult ToolPartKind = "result"
)

// ToolPart is an artifact part that reports a tool call or a tool result, as a dialect
// recognizes it. Both are untrusted, like every value the agent sends.
type ToolPart struct {
	Kind ToolPartKind   `json:"kind"`
	ID   string         `json:"id"`   // the call's id; a result repeats it
	Name string         `json:"name"` // the tool's name
	Args map[string]any `json:"args"` // ToolCall: the arguments
	// Result is what a ToolResult returned.
	Result any `json:"result"`
}

// Nested says a sub-agent raised the interaction, and names the path to it.
type Nested struct {
	Subagent  string `json:"subagent"`
	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id"`
}

// Interaction is a pause the agent needs a person to answer. It holds what the dialect
// needs to build the answer, so keep it unchanged from the parse to the reply. Interaction and
// Answer are stored as JSON, so their tags are a stored format: never rename one (interaction_test.go
// pins them).
type Interaction struct {
	// TextReplies permits queued plain-text answers to this question.
	TextReplies bool       `json:"text_replies,omitempty"`
	Kind        Kind       `json:"kind"`
	TaskID      a2a.TaskID `json:"task_id"` // the paused task, which the answer is addressed to
	ContextID   string     `json:"context_id"`
	// Hint is the agent's own one-line description of the pause, when it sends one. It is
	// untrusted text, like every question and tool argument.
	Hint      string     `json:"hint"`
	RequestID string     `json:"request_id"` // AskUser: the id the answer must echo (for kagent-v1 nested, the child tool's)
	Questions []Question `json:"questions"`  // AskUser
	Tools     []Tool     `json:"tools"`      // ToolApproval
	Nested    *Nested    `json:"nested"`     // nil unless a sub-agent raised it
}

// Decision is a person's verdict on one tool call.
type Decision struct {
	ToolID   string `json:"tool_id"` // Tool.ID
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"` // why it was rejected; ignored when approved
}

// Answer is a person's reply to an Interaction.
type Answer struct {
	// RequestID echoes Interaction.RequestID, so a reply to an older pause is refused.
	RequestID string `json:"request_id"`
	// Answers holds, for each question in order, the chosen choices or the free text.
	Answers [][]string `json:"answers"`
	// Decisions holds one verdict for each tool of a ToolApproval.
	Decisions []Decision `json:"decisions"`
}
