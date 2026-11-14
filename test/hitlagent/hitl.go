package hitlagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// ExtensionURI is kagent's human-in-the-loop extension, the one approval contract Agent Kourier reads.
const ExtensionURI = "https://kagent.dev/extensions/hitl/v1"

const (
	typeToolApprovalRequest  = "tool_approval_request"
	typeToolApprovalResponse = "tool_approval_response"
)

// ToolApprovalRequest is the pause payload the agent puts under ExtensionURI in its input-required status message.
type ToolApprovalRequest struct {
	Type  string     `json:"type"`
	Hint  string     `json:"hint,omitempty"`
	Tools []ToolCall `json:"tools"`
}

// ToolCall is one tool call that waits for a decision.
type ToolCall struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id,omitempty"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// ToolApprovalResponse is the answer payload Agent Kourier sends under ExtensionURI in the answer's metadata.
type ToolApprovalResponse struct {
	Type      string     `json:"type"`
	Approvals []Approval `json:"approvals"`
}

// Approval is the decision on one tool, matched to the request by ID.
type Approval struct {
	ID              string `json:"id"`
	Approved        *bool  `json:"approved"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// decision pairs a requested tool call with the verdict the answer gave it.
type decision struct {
	tool     ToolCall
	approved bool
	reason   string
}

// errMalformedAnswer marks an answer that does not decide the paused request.
var errMalformedAnswer = errors.New("malformed tool approval answer")

// toMetadataValue turns a payload into the JSON-shaped map[string]any the task store and the wire both carry.
func toMetadataValue(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode metadata value: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode metadata value: %w", err)
	}
	return out, nil
}

// fromMetadataValue decodes a metadata value (JSON-shaped) into dst.
func fromMetadataValue(v, dst any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode metadata value: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode metadata value: %w", err)
	}
	return nil
}

// pendingApproval returns the approval request a task is paused on, if it is paused on one.
func pendingApproval(task *a2a.Task) (ToolApprovalRequest, bool) {
	if task == nil || task.Status.State != a2a.TaskStateInputRequired || task.Status.Message == nil {
		return ToolApprovalRequest{}, false
	}
	msg := task.Status.Message
	if !slices.Contains(msg.Extensions, ExtensionURI) {
		return ToolApprovalRequest{}, false
	}
	raw, ok := msg.Metadata[ExtensionURI]
	if !ok {
		return ToolApprovalRequest{}, false
	}
	var req ToolApprovalRequest
	if err := fromMetadataValue(raw, &req); err != nil || req.Type != typeToolApprovalRequest || len(req.Tools) == 0 {
		return ToolApprovalRequest{}, false
	}
	return req, true
}

// parseApprovalResponse checks an answer against the paused request: every requested tool decided exactly once,
// by its id, and no decision for a tool that was not asked about. It returns the decisions in request order.
func parseApprovalResponse(req ToolApprovalRequest, msg *a2a.Message) ([]decision, error) {
	if msg == nil {
		return nil, fmt.Errorf("%w: no message", errMalformedAnswer)
	}
	raw, ok := msg.Metadata[ExtensionURI]
	if !ok {
		return nil, fmt.Errorf("%w: no payload under %s in the message metadata", errMalformedAnswer, ExtensionURI)
	}
	var resp ToolApprovalResponse
	if err := fromMetadataValue(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: %w", errMalformedAnswer, err)
	}
	if resp.Type != typeToolApprovalResponse {
		return nil, fmt.Errorf("%w: type %q, want %q", errMalformedAnswer, resp.Type, typeToolApprovalResponse)
	}

	byID := make(map[string]Approval, len(resp.Approvals))
	for _, a := range resp.Approvals {
		if a.ID == "" {
			return nil, fmt.Errorf("%w: an approval has no id", errMalformedAnswer)
		}
		if !slices.ContainsFunc(req.Tools, func(t ToolCall) bool { return t.ID == a.ID }) {
			return nil, fmt.Errorf("%w: approval for tool id %q, which was not asked about", errMalformedAnswer, a.ID)
		}
		if _, dup := byID[a.ID]; dup {
			return nil, fmt.Errorf("%w: tool id %q decided twice", errMalformedAnswer, a.ID)
		}
		if a.Approved == nil {
			return nil, fmt.Errorf("%w: tool id %q has no approved flag", errMalformedAnswer, a.ID)
		}
		byID[a.ID] = a
	}

	out := make([]decision, 0, len(req.Tools))
	for _, t := range req.Tools {
		a, ok := byID[t.ID]
		if !ok {
			return nil, fmt.Errorf("%w: tool id %q not decided", errMalformedAnswer, t.ID)
		}
		out = append(out, decision{tool: t, approved: *a.Approved, reason: strings.TrimSpace(a.RejectionReason)})
	}
	return out, nil
}
