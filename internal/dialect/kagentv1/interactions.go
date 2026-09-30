package kagentv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

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

// wireAnswer, wireApproval and the two response payloads are what kagent accepts as a
// reply (measured at alpha5, WP-02). The nested shapes are the same (kagent source).
type wireAnswer struct {
	Answer []string `json:"answer"`
}

type askUserResponse struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Answers []wireAnswer `json:"answers"`
}

type wireApproval struct {
	ID              string `json:"id"`
	Approved        bool   `json:"approved"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

type toolApprovalResponse struct {
	Type      string         `json:"type"`
	Approvals []wireApproval `json:"approvals"`
}

// BuildResponse returns the user message that answers in: the task and context of the
// pause, the HITL extension, and the payload kagent expects. It checks the answer first,
// because kagent accepts an incomplete or off-choice answer, and wedges the task on an
// empty answer list or a wrong request id (spec 6.3, R17). Every question needs an answer
// that is one of its choices (when it has any), and only one unless it is multi-select;
// every tool needs exactly one decision; the request id must be in's. Otherwise it
// returns an error and no message. The A2A-Extensions header comes from Connect.
func (*Dialect) BuildResponse(in dialect.Interaction, answer dialect.Answer) (a2a.Message, error) {
	if in.TaskID == "" {
		return a2a.Message{}, errors.New("interaction has no task to answer")
	}
	var payload any
	var text string
	var err error
	switch in.Kind {
	case dialect.AskUser:
		payload, text, err = askUserAnswer(in, answer)
	case dialect.ToolApproval:
		payload, text, err = toolApprovalAnswer(in, answer)
	default:
		err = fmt.Errorf("unknown interaction kind %q", in.Kind)
	}
	if err != nil {
		return a2a.Message{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = in.TaskID, in.ContextID
	msg.Extensions = []string{HITLExtension}
	msg.Metadata = map[string]any{HITLExtension: payload}
	return *msg, nil
}

func askUserAnswer(in dialect.Interaction, a dialect.Answer) (askUserResponse, string, error) {
	switch {
	case in.RequestID == "":
		return askUserResponse{}, "", errors.New("interaction has no request id")
	case a.RequestID != in.RequestID:
		return askUserResponse{}, "", errors.New("answer is for a different request")
	case len(a.Decisions) > 0:
		return askUserResponse{}, "", errors.New("tool decisions cannot answer questions")
	case len(a.Answers) != len(in.Questions):
		return askUserResponse{}, "", fmt.Errorf("%d of %d questions answered", len(a.Answers), len(in.Questions))
	}
	resp := askUserResponse{Type: "ask_user_response", ID: in.RequestID}
	var flat []string
	for i, q := range in.Questions {
		if err := checkAnswer(q, a.Answers[i]); err != nil {
			return askUserResponse{}, "", fmt.Errorf("question %d: %w", i+1, err)
		}
		resp.Answers = append(resp.Answers, wireAnswer{Answer: a.Answers[i]})
		flat = append(flat, strings.Join(a.Answers[i], ", "))
	}
	return resp, strings.Join(flat, "; "), nil
}

// checkAnswer reports why vals does not answer q.
func checkAnswer(q dialect.Question, vals []string) error {
	if len(vals) == 0 {
		return errors.New("not answered")
	}
	if len(vals) > 1 && !q.Multiple {
		return errors.New("only one answer is allowed")
	}
	for i, v := range vals {
		switch {
		case strings.TrimSpace(v) == "":
			return errors.New("blank answer")
		case len(q.Choices) > 0 && !slices.Contains(q.Choices, v):
			return fmt.Errorf("%q is not one of the choices", v)
		case slices.Contains(vals[:i], v):
			return fmt.Errorf("%q is chosen twice", v)
		}
	}
	return nil
}

func toolApprovalAnswer(in dialect.Interaction, a dialect.Answer) (toolApprovalResponse, string, error) {
	if len(a.Answers) > 0 {
		return toolApprovalResponse{}, "", errors.New("question answers cannot decide tool calls")
	}
	if len(a.Decisions) != len(in.Tools) {
		return toolApprovalResponse{}, "", fmt.Errorf("%d of %d tool calls decided", len(a.Decisions), len(in.Tools))
	}
	byID := make(map[string]dialect.Decision, len(a.Decisions))
	for _, d := range a.Decisions {
		if !slices.ContainsFunc(in.Tools, func(t dialect.Tool) bool { return t.ID == d.ToolID }) {
			return toolApprovalResponse{}, "", errors.New("decision for a tool call that is not pending")
		}
		if _, twice := byID[d.ToolID]; twice {
			return toolApprovalResponse{}, "", errors.New("a tool call is decided twice")
		}
		byID[d.ToolID] = d
	}
	resp, text := toolApprovalResponse{Type: "tool_approval_response"}, "approved"
	for _, t := range in.Tools { // in request order, one decision each
		d := byID[t.ID]
		w := wireApproval{ID: t.ID, Approved: d.Approved}
		if !d.Approved {
			w.RejectionReason, text = d.Reason, "rejected"
		}
		resp.Approvals = append(resp.Approvals, w)
	}
	return resp, text, nil
}
