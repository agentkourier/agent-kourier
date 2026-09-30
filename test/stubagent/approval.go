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
	var issued []string           // the approval ids, in request order
	refs := map[string]Approval{} // by approval id
	results := map[string]obj{}   // by call id: what the tool returns if it runs
	ok := r.endText()
	for _, tool := range a.tools {
		call, id := r.s.newID("call"), r.s.newID("adk")
		issued = append(issued, id)
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
			decode:  decodeApprovals(issued, refs),
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

// decodeApprovals reads a tool-approval reply to the requests issued, in order, with refs
// giving each by id. Measured (WP-02): a reply with no decisions is plain text, so the tool
// never runs (approval/08); and a reply that lacks the issued id is answered -32603 "tool
// approval response is missing id <the issued id>" (approval/11, whose reply carried "nope"
// instead). The stub looks for each issued id, in request order, and names the first it does not
// find.
// UNCONFIRMED: that rule for several tools, inferred from that error text; a reply that decides
// every issued tool and also names another id, which the stub rejects, stricter than the
// fixtures show; a decision without an approved field, which the stub reads as a rejection;
// and a reply with another or no type, or sent without the extension header, taken as plain.
func decodeApprovals(issued []string, refs map[string]Approval) func(obj) (Response, *rpcError) {
	return func(payload obj) (Response, *rpcError) {
		given, _ := payload["approvals"].([]any)
		if payload["type"] != "tool_approval_response" || len(given) == 0 {
			return Response{}, nil
		}
		byID := map[string]any{}
		for _, g := range given {
			id, _ := dig(g, "id").(string)
			byID[id] = g
		}
		for _, id := range issued {
			if byID[id] == nil {
				return Response{}, internalError("tool approval response is missing id %s", id)
			}
		}
		var out []Approval
		for _, g := range given {
			id, _ := dig(g, "id").(string)
			ap, known := refs[id]
			if !known {
				return Response{}, internalError("tool approval response has unexpected id %s", id)
			}
			ap.Approved, _ = dig(g, "approved").(bool)
			ap.Reason, _ = dig(g, "rejection_reason").(string)
			out = append(out, ap)
		}
		return Response{Approvals: out}, nil
	}
}
