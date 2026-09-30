package kagentv1

import (
	"encoding/json"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// The HITL extension's payload types, as sent in a paused task's status message metadata.
const (
	askUserRequest      = "ask_user_request"
	toolApprovalRequest = "tool_approval_request"
)

// wireTool, wireQuestion, wireNested and hitlRequest mirror kagent's request payloads
// (go/api/a2a/hitl.go). Measured at alpha5 (WP-02): the ask-user and tool-approval
// requests, in test/fixtures/kagent/askuser and approval. Source only, never captured:
// the nested object, which either request kind may carry.
type wireTool struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

type wireQuestion struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Multiple bool     `json:"multiple"`
}

type wireNested struct {
	SubagentName string     `json:"subagent_name"`
	TaskID       string     `json:"task_id"`
	ContextID    string     `json:"context_id"`
	Tools        []wireTool `json:"tools"`
}

type hitlRequest struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Questions []wireQuestion `json:"questions"`
	Tools     []wireTool     `json:"tools"`
	Nested    *wireNested    `json:"nested"`
}

// ParseInteraction returns the interaction a task paused on. It reports false unless the
// task is input-required with a usable HITL request: an ask-user request needs an id and a
// question, and a tool-approval request needs tools that each have an id. A pause without
// the extension header has no payload and is not parsed (the header comes from Connect).
//
// For a nested tool approval, the tools to decide are the nested request's, which is where
// kagent's source matches decisions; that is unconfirmed against a live run.
func (*Dialect) ParseInteraction(task a2a.Task) (*dialect.Interaction, bool) {
	msg := task.Status.Message
	if task.Status.State != a2a.TaskStateInputRequired || msg == nil || msg.Metadata[HITLExtension] == nil {
		return nil, false
	}
	raw, err := json.Marshal(msg.Metadata[HITLExtension])
	var req hitlRequest
	if err != nil || json.Unmarshal(raw, &req) != nil {
		return nil, false
	}
	in := &dialect.Interaction{TaskID: task.ID, ContextID: task.ContextID}
	if n := req.Nested; n != nil {
		in.Nested = &dialect.Nested{Subagent: n.SubagentName, TaskID: n.TaskID, ContextID: n.ContextID}
	}
	switch req.Type {
	case askUserRequest:
		in.Kind, in.RequestID = dialect.AskUser, req.ID
		for _, q := range req.Questions {
			in.Questions = append(in.Questions, dialect.Question{Text: q.Question, Choices: append([]string(nil), q.Choices...), Multiple: q.Multiple})
		}
		if in.RequestID == "" || len(in.Questions) == 0 {
			return nil, false
		}
		return in, true
	case toolApprovalRequest:
		in.Kind = dialect.ToolApproval
		tools := req.Tools
		if req.Nested != nil && len(req.Nested.Tools) > 0 {
			tools = req.Nested.Tools
		}
		for _, w := range tools {
			if w.ID == "" {
				return nil, false
			}
			in.Tools = append(in.Tools, dialect.Tool{ID: w.ID, CallID: w.CallID, Name: w.Name, Args: w.Args})
		}
		if len(in.Tools) == 0 {
			return nil, false
		}
		return in, true
	}
	return nil, false
}
