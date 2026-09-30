package stubagent_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const addHint = "Please approve or reject the tool call add_numbers() by responding with a FunctionResponse with an expected ToolConfirmation payload. (add_numbers)"

// addNumbers is the spike's MCP tool with requireApproval: it adds 3 and 5.
func addNumbers() stubagent.ToolRequest {
	return stubagent.ToolRequest{Name: "add_numbers", Args: obj{"a": 3, "b": 5}, Result: obj{"output": "8"}}
}

// approval is the HITL payload of a decision on one tool.
func approval(id string, approved bool, reason string) obj {
	a := obj{"id": id, "approved": approved}
	if reason != "" {
		a["rejection_reason"] = reason
	}
	return a
}

// decisions is the HITL payload of a tool-approval reply.
func decisions(as ...obj) obj {
	if as == nil {
		as = []obj{}
	}
	return obj{"type": "tool_approval_response", "approvals": as}
}

// toolIDs returns the ids of the tools in a tool_approval_request.
func toolIDs(request obj) []string {
	var ids []string
	tools, _ := request["tools"].([]any)
	for _, tool := range tools {
		id, _ := dig(tool, "id").(string)
		ids = append(ids, id)
	}
	return ids
}

func TestApprovalPausesLikeKagent(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.RequestApproval([]stubagent.ToolRequest{addNumbers()}, nil))

	evs, _, ctxID, request := paused(t, withHITL(c))

	rec := fixture(t, "approval/01-pause-approve.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	for i := range evs {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	tools, _ := request["tools"].([]any)
	if request["type"] != "tool_approval_request" || request["hint"] != addHint || len(tools) != 1 ||
		dig(tools[0], "name") != "add_numbers" || dig(tools[0], "args", "b") != float64(5) || dig(tools[0], "id") == dig(tools[0], "call_id") {
		t.Errorf("request = %v, want add_numbers(3, 5) with its own id and a call_id", request)
	}
	msgOf, _ := dig(last(evs), "result", "task", "status", "message").(obj)
	if text(msgOf["parts"]) != addHint || msgOf["contextId"] != ctxID || !reflect.DeepEqual(msgOf["extensions"], []any{hitlURI}) {
		t.Errorf("status message = %v, want ADK's hint as the text and the extension URI", msgOf)
	}
}

func TestApprovalWithoutTheExtensionHasOnlyADKsHint(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.RequestApproval([]stubagent.ToolRequest{addNumbers()}, nil))

	evs, _, _, request := paused(t, c)

	expectLike(t, "paused task", last(evs), last(fixture(t, "approval/13-ask-without-extension-header.response.sse")))
	if request != nil || text(dig(last(evs), "result", "task", "status", "message", "parts")) != addHint {
		t.Errorf("paused task = %v, want the hint as text and no payload", dig(last(evs), "result", "task", "status", "message"))
	}
}

func TestDecisionsRunOrRefuseTheTool(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approved bool
		reason   string
		fixture  string
		response obj // the function_response's data.response
	}{
		{"approve", true, "", "approval/02-approve.response.sse", obj{"output": "8"}},
		{"reject with a reason", false, "not today", "approval/05-reject-with-reason.response.sse", obj{"error": `error tool "add_numbers" call is rejected`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, agent, c := newAgent(t)
			var seen stubagent.Response
			agent.Next(stubagent.RequestApproval([]stubagent.ToolRequest{addNumbers()}, func(r stubagent.Response) []stubagent.Step {
				seen = r
				return []stubagent.Step{stubagent.Text("done")}
			}))
			c = withHITL(c)
			_, taskID, ctxID, request := paused(t, c)

			evs := c.send(reply("m2", "yes", ctxID, taskID, decisions(approval(toolIDs(request)[0], tc.approved, tc.reason))))

			rec := fixture(t, tc.fixture)
			if got, want := shape(evs), shape(rec); got != want {
				t.Fatalf("shape = %s, want %s", got, want)
			}
			for i := range evs {
				expectLike(t, describe(evs[i]), evs[i], rec[i])
			}
			if got := dig(firstPart(evs[3]), "data", "response"); !reflect.DeepEqual(got, tc.response) {
				t.Errorf("tool result = %v, want %v", got, tc.response)
			}
			want := []stubagent.Approval{{ID: toolIDs(request)[0], Name: "add_numbers", Approved: tc.approved, Reason: tc.reason}}
			if len(seen.Approvals) != 1 || seen.Approvals[0].CallID == "" || seen.Plain() {
				t.Fatalf("the script saw %+v, want one decision", seen)
			}
			seen.Approvals[0].CallID = ""
			if !reflect.DeepEqual(seen.Approvals, want) {
				t.Errorf("the script saw %+v, want %+v", seen.Approvals, want)
			}
		})
	}
}

func TestSeveralToolsInOnePause(t *testing.T) {
	_, agent, c := newAgent(t)
	var seen stubagent.Response
	agent.Next(stubagent.RequestApproval([]stubagent.ToolRequest{
		addNumbers(), {Name: "delete_all", Args: obj{}, Result: obj{"output": "gone"}},
	}, func(r stubagent.Response) []stubagent.Step { seen = r; return nil }))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	tools := toolIDs(request)

	evs := c.send(reply("m2", "x", ctxID, taskID, decisions(approval(tools[0], true, ""), approval(tools[1], false, ""))))

	if len(tools) != 2 || !strings.Contains(request["hint"].(string), "add_numbers") || !strings.Contains(request["hint"].(string), "delete_all") {
		t.Fatalf("request = %v, want two tools named in the hint", request)
	}
	if got, want := shape(evs), "statusUpdate:WORKING x3, artifact+last x2, task:COMPLETED"; got != want {
		t.Errorf("shape = %s, want %s", got, want)
	}
	if len(seen.Approvals) != 2 || !seen.Approvals[0].Approved || seen.Approvals[1].Approved || seen.Approvals[1].Name != "delete_all" {
		t.Errorf("the script saw %+v, want add_numbers approved then delete_all refused", seen.Approvals)
	}
}

func TestEmptyApprovalsAreAPlainReply(t *testing.T) {
	_, agent, c := newAgent(t)
	var seen stubagent.Response
	agent.Next(stubagent.RequestApproval([]stubagent.ToolRequest{addNumbers()}, func(r stubagent.Response) []stubagent.Step {
		seen = r
		return []stubagent.Step{stubagent.Text("Saw a plain user message.")}
	}))
	c = withHITL(c)
	_, taskID, ctxID, _ := paused(t, c)

	evs := c.send(reply("m2", "x", ctxID, taskID, decisions()))

	rec := fixture(t, "approval/08-bad-empty-approvals.response.sse")
	if got, want := shape(evs), shape(rec); got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	if !seen.Plain() {
		t.Errorf("the script saw %+v, want a plain reply (the shape shows the tool did not run)", seen)
	}
}

// badApproval sends a reply with the decisions to a fresh pause for the tools, and returns its
// stream, the task id and the approval ids the request issued.
func badApproval(t *testing.T, tools []stubagent.ToolRequest, pick func(issued []string) []obj) (st *stream, taskID string, issued []string, called *bool) {
	t.Helper()
	_, agent, c := newAgent(t)
	called = new(bool)
	agent.Next(stubagent.RequestApproval(tools, func(stubagent.Response) []stubagent.Step { *called = true; return nil }))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)
	issued = toolIDs(request)
	st = c.open("SendStreamingMessage", reply("m2", "x", ctxID, taskID, decisions(pick(issued)...)))
	return st, taskID, issued, called
}

// TestApprovalWithoutAnIssuedIDWedgesTheTask: approval/11 answers a reply that carries "nope" and
// not the issued id with "tool approval response is missing id <the issued id>": kagent looks
// for each issued id in the reply and names the one it does not find.
func TestApprovalWithoutAnIssuedIDWedgesTheTask(t *testing.T) {
	st, _, issued, called := badApproval(t, []stubagent.ToolRequest{addNumbers()},
		func([]string) []obj { return []obj{approval("nope", true, "")} })
	evs := st.all()

	rec := fixture(t, "approval/11-bad-unknown-id.response.sse")
	if got, want := shape(evs), "statusUpdate:WORKING, error:-32603"; got != want || st.status != 200 || !st.sse {
		t.Fatalf("unknown id = HTTP %d, sse %v, %s; want 200, true, %s", st.status, st.sse, got, want)
	}
	expectLike(t, "error", evs[1], rec[1])
	if got, want := dig(evs[1], "error", "message"), "tool approval response is missing id "+issued[0]; got != want || *called {
		t.Errorf("error message = %v, script called %v; want %q", got, *called, want)
	}
}

// TestPartialApprovalNamesTheFirstIssuedIDMissing: with two tools, a reply that decides only
// one is missing the other's id, so it wedges the task like the reply in approval/11.
// UNCONFIRMED: that rule for several tools is inferred from that error text; the fixtures have
// one tool.
func TestPartialApprovalNamesTheFirstIssuedIDMissing(t *testing.T) {
	two := []stubagent.ToolRequest{addNumbers(), {Name: "delete_all", Args: obj{}, Result: obj{}}}
	for _, tc := range []struct {
		name    string
		decided int // the index of the tool the reply decides
		missing int // the index of the tool it names as missing
	}{
		{"the first", 0, 1},
		{"the second", 1, 0},
	} {
		st, _, issued, called := badApproval(t, two, func(issued []string) []obj { return []obj{approval(issued[tc.decided], true, "")} })
		evs := st.all()

		if got, want := shape(evs), "statusUpdate:WORKING, error:-32603"; got != want {
			t.Errorf("reply deciding %s tool = %s, want %s", tc.name, got, want)
			continue
		}
		if got, want := dig(evs[1], "error", "message"), "tool approval response is missing id "+issued[tc.missing]; got != want || *called {
			t.Errorf("reply deciding %s tool: error %v, script called %v; want %q", tc.name, got, *called, want)
		}
	}
}

// TestUnexpectedApprovalIDIsRejected: a reply that decides every issued tool and one more.
// UNCONFIRMED: kagent's answer; the stub is stricter than the fixtures show and rejects it.
func TestUnexpectedApprovalIDIsRejected(t *testing.T) {
	st, _, _, called := badApproval(t, []stubagent.ToolRequest{addNumbers()},
		func(issued []string) []obj { return []obj{approval(issued[0], true, ""), approval("nope", true, "")} })

	evs := st.all()

	if got, want := shape(evs), "statusUpdate:WORKING, error:-32603"; got != want || *called {
		t.Errorf("reply with an extra id = %s, script called %v; want %s and the script not called", got, *called, want)
	}
}

// TestDecisionWithoutApprovedIsARejection: UNCONFIRMED: a decision with no approved field is
// taken as a rejection, the safe reading; no fixture omits it.
func TestDecisionWithoutApprovedIsARejection(t *testing.T) {
	_, agent, c := newAgent(t)
	var seen stubagent.Response
	agent.Next(stubagent.RequestApproval([]stubagent.ToolRequest{addNumbers()}, func(r stubagent.Response) []stubagent.Step { seen = r; return nil }))
	c = withHITL(c)
	_, taskID, ctxID, request := paused(t, c)

	c.send(reply("m2", "x", ctxID, taskID, decisions(obj{"id": toolIDs(request)[0]})))

	if len(seen.Approvals) != 1 || seen.Approvals[0].Approved {
		t.Errorf("the script saw %+v, want one decision, not approved", seen.Approvals)
	}
}
