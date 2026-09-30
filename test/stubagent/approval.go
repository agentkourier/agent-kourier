package stubagent

import (
	"fmt"
	"strings"
)

// ToolRequest is a tool call the agent wants approved.
type ToolRequest struct {
	Name   string
	Args   map[string]any
	Result map[string]any // what the tool returns if it is approved and runs
}

// Approval is the user's decision on one tool call.
type Approval struct {
	ID       string // the approval id from the request
	CallID   string // the id of the tool call
	Name     string
	Approved bool
	Reason   string // the rejection reason, if the user gave one
}

// RequestApproval ends the turn with a tool-approval request, as kagent does for an MCP tool
// with requireApproval: the tool calls, each answered "requires confirmation", then an
// INPUT_REQUIRED task. Steps after it do not run. When the user decides, each tool's result
// follows, its Result if approved and kagent's rejection error if not, and then the steps
// that then returns.
//
// As with Ask, the request reaches only a client that named HITLExtension; any other client
// sees ADK's hint sentence as the status text (WP-02, measured).
// UNCONFIRMED: several tools in one pause. The hint lists their names, and a reply may decide
// any subset of them.
func RequestApproval(tools []ToolRequest, then func(Response) []Step) Step {
	return approvalStep{tools, then}
}

type approvalStep struct {
	tools []ToolRequest
	then  func(Response) []Step
}

func (a approvalStep) play(r *run) bool {
	var requested []obj
	var names []string
	refs := map[string]Approval{} // by approval id
	results := map[string]obj{}   // by call id: what the tool returns if it runs
	ok := r.endText()
	for _, tool := range a.tools {
		call, id := r.s.newID("call"), r.s.newID("adk")
		refs[id], results[call] = Approval{ID: id, CallID: call, Name: tool.Name}, tool.Result
		requested = append(requested, obj{"id": id, "call_id": call, "name": tool.Name, "args": tool.Args})
		names = append(names, tool.Name)
		ok = ok && r.data(obj{"id": call, "name": tool.Name, "args": tool.Args}, "function_call") &&
			r.data(obj{"id": call, "name": tool.Name, "response": obj{
				"error": fmt.Sprintf("error tool %q requires confirmation, please approve or reject", tool.Name),
			}}, "function_response")
	}
	hint := fmt.Sprintf("Please approve or reject the tool call %s() by responding with a FunctionResponse with an expected ToolConfirmation payload. (%s)",
		strings.Join(names, "(), "), strings.Join(names, ", "))
	if ok {
		r.pause(&pause{
			msgID: r.s.newID("message"), text: hint,
			request: obj{"type": "tool_approval_request", "hint": hint, "tools": requested},
			decode:  decodeApprovals(refs),
			then: func(resp Response) []Step {
				var steps []Step
				for _, ap := range resp.Approvals {
					result := results[ap.CallID]
					if !ap.Approved {
						result = obj{"error": fmt.Sprintf("error tool %q call is rejected", ap.Name)}
					}
					steps = append(steps, responseStep{ap.CallID, ap.Name, result})
				}
				if a.then != nil {
					steps = append(steps, a.then(resp)...)
				}
				return steps
			},
		})
	}
	return false
}

// decodeApprovals reads a tool-approval reply. kagent treats a reply with no decisions as plain
// text, so the tool never runs, and rejects one that names an id it did not issue (WP-02,
// measured).
func decodeApprovals(refs map[string]Approval) func(obj) (Response, *rpcError) {
	return func(payload obj) (Response, *rpcError) {
		given, _ := payload["approvals"].([]any)
		if payload["type"] != "tool_approval_response" || len(given) == 0 {
			return Response{}, nil
		}
		var out []Approval
		for _, g := range given {
			id, _ := dig(g, "id").(string)
			ap, known := refs[id]
			if !known {
				return Response{}, internalError("tool approval response is missing id %s", id)
			}
			ap.Approved, _ = dig(g, "approved").(bool)
			ap.Reason, _ = dig(g, "rejection_reason").(string)
			out = append(out, ap)
		}
		return Response{Approvals: out}, nil
	}
}
