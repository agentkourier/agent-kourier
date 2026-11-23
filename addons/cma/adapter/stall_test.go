package adapter

import (
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

const permissionAllow = "allow"

func newStallHandler(t *testing.T, fake *fakeBackend, timeout time.Duration) *a2aclient.Client {
	t.Helper()
	st := newStore(t, nil)
	return serve(t, &Executor{Backend: fake, Store: st, Keepalive: time.Hour, ToolResultTimeout: timeout}, st, nil)
}

func statusText(task *a2a.Task) string {
	if task.Status.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range task.Status.Message.Parts {
		if s, ok := p.Content.(a2a.Text); ok {
			b.WriteString(string(s))
		}
	}
	return b.String()
}

func interrupted(fake *fakeBackend) bool {
	for _, s := range fake.sentEvents() {
		if s.out.Interrupt {
			return true
		}
	}
	return false
}

// A sandbox that never starts: the allowed tool call gets no result.
func TestToolCallWithNoResultFailsTheTurn(t *testing.T) {
	fake := newFake(script(toolUse("sevt_read", "read", map[string]any{"file_path": "/etc/hostname"}, permissionAllow)))
	task, _ := turn(t, newStallHandler(t, fake, 100*time.Millisecond), userText("", "read it"))

	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.Status.State)
	}
	if msg := statusText(task); !strings.Contains(msg, "sandbox") || !strings.Contains(msg, "100ms") {
		t.Errorf("status message = %q, want it to name the sandbox and the timeout", msg)
	}
	if !interrupted(fake) {
		t.Error("the stalled session was not interrupted")
	}
	// Agent Kourier shows a backend's failure only as a fixed notice; this code selects its timeout notice.
	if code := task.Status.Message.Metadata["code"]; code != "timeout" {
		t.Errorf("status metadata code = %v, want timeout", code)
	}
}

// The sandbox stalls after a person approves the call.
func TestApprovedToolCallWithNoResultFailsTheTurn(t *testing.T) {
	fake := newFake(func(session string, out Outbound) []Event {
		if out.Confirmation != nil {
			return nil // the worker never claims the call
		}
		return approvalScript(session, out)
	})
	h := newStallHandler(t, fake, 100*time.Millisecond)
	paused, _ := turn(t, h, userText("", "clean up"))
	if paused.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s, want input-required", paused.Status.State)
	}
	done, _ := turn(t, h, approvalReply(paused, true, ""))
	if done.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want failed", done.Status.State)
	}
	if !interrupted(fake) {
		t.Error("the stalled session was not interrupted")
	}
}

// A denied call runs nothing in the sandbox, so its slow follow-up is not a stall.
func TestDeniedToolCallIsNotWaitedOn(t *testing.T) {
	fake := newFake(approvalScript)
	fake.respond = func(session string, out Outbound) []Event {
		if out.Confirmation != nil && !out.Confirmation.Allow {
			go func() {
				time.Sleep(300 * time.Millisecond)
				fake.publish(session, running(), say("Understood."), idle(stopEndTurn))
			}()
			return nil
		}
		return approvalScript(session, out)
	}
	h := newStallHandler(t, fake, 100*time.Millisecond)
	paused, _ := turn(t, h, userText("", "clean up"))
	done, _ := turn(t, h, approvalReply(paused, false, "no"))
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s (%q), want completed", done.Status.State, statusText(done))
	}
}

// The model can be silent for longer than the timeout; only a wait on the sandbox counts.
func TestSilentModelIsNotAStall(t *testing.T) {
	fake := newFake(nil)
	fake.respond = func(session string, _ Outbound) []Event {
		go func() {
			time.Sleep(300 * time.Millisecond)
			fake.publish(session, say("Thought hard."), idle(stopEndTurn))
		}()
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running()}
	}
	task, _ := turn(t, newStallHandler(t, fake, 100*time.Millisecond), userText("", "think"))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s (%q), want completed", task.Status.State, statusText(task))
	}
}

// A tool result ends the wait: the model's silence after it is not a stall.
func TestToolResultEndsTheWait(t *testing.T) {
	fake := newFake(nil)
	fake.respond = func(session string, _ Outbound) []Event {
		go func() {
			time.Sleep(50 * time.Millisecond)
			fake.publish(session, Event{Type: evUserToolResult, ToolUseID: "sevt_read", Content: []ContentBlock{{Type: "text", Text: "host"}}})
			time.Sleep(300 * time.Millisecond)
			fake.publish(session, running(), say("It is host."), idle(stopEndTurn))
		}()
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(), toolUse("sevt_read", "read", nil, permissionAllow)}
	}
	task, _ := turn(t, newStallHandler(t, fake, 100*time.Millisecond), userText("", "read it"))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s (%q), want completed", task.Status.State, statusText(task))
	}
}

func TestZeroToolResultTimeoutWaits(t *testing.T) {
	fake := newFake(nil)
	fake.respond = func(session string, _ Outbound) []Event {
		go func() {
			time.Sleep(200 * time.Millisecond)
			fake.publish(session, Event{Type: evUserToolResult, ToolUseID: "sevt_read"}, running(), say("ok"), idle(stopEndTurn))
		}()
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(), toolUse("sevt_read", "read", nil, permissionAllow)}
	}
	task, _ := turn(t, newStallHandler(t, fake, 0), userText("", "read it"))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}
}

// An MCP tool does not run in the sandbox, so waiting on it is not a sandbox stall.
func TestMCPToolIsNotASandboxWait(t *testing.T) {
	fake := newFake(nil)
	fake.respond = func(session string, _ Outbound) []Event {
		go func() {
			time.Sleep(300 * time.Millisecond)
			fake.publish(session, Event{Type: evAgentMCPResult, MCPToolUseID: "sevt_mcp"}, say("ok"), idle(stopEndTurn))
		}()
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(),
			{ID: "sevt_mcp", Type: evAgentMCPToolUse, Name: "search", EvaluatedPermission: permissionAllow}}
	}
	task, _ := turn(t, newStallHandler(t, fake, 100*time.Millisecond), userText("", "search"))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s (%q), want completed", task.Status.State, statusText(task))
	}
}

// On a self-hosted environment an allowed built-in tool also idles the session with requires_action, naming the
// call, until the worker posts its result. That is a wait, not a question for a person.
func TestAllowedSandboxToolWaitsForTheWorker(t *testing.T) {
	fake := newFake(nil)
	fake.respond = func(session string, _ Outbound) []Event {
		go func() {
			time.Sleep(50 * time.Millisecond)
			fake.publish(session, Event{Type: evUserToolResult, ToolUseID: "sevt_read", Content: []ContentBlock{{Type: "text", Text: "sesn-x"}}},
				running(), say("sesn-x"), idle(stopEndTurn))
		}()
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(),
			toolUse("sevt_read", "read", map[string]any{"file_path": "/etc/hostname"}, permissionAllow),
			idle(stopRequiresAction, "sevt_read")}
	}
	task, _ := turn(t, newStallHandler(t, fake, time.Second), userText("", "read it"))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s (%q), want completed", task.Status.State, statusText(task))
	}
	if !strings.Contains(artifactText(task), "sesn-x") {
		t.Errorf("text = %q", artifactText(task))
	}
}

// A pause naming an allowed call and one that needs approval asks only about the second.
func TestPauseAsksOnlyAboutCallsNeedingApproval(t *testing.T) {
	fake := newFake(script(
		toolUse("sevt_read", "read", nil, permissionAllow),
		toolUse("sevt_ask", "bash", map[string]any{"command": "date"}, permissionAsk),
		idle(stopRequiresAction, "sevt_read", "sevt_ask"),
	))
	task, _ := turn(t, newStallHandler(t, fake, time.Second), userText("", "both"))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s (%q), want input-required", task.Status.State, statusText(task))
	}
	tools, _ := hitlRequest(t, task)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want only the bash call", tools)
	}
	if tool, _ := tools[0].(map[string]any); tool["id"] != "sevt_ask" {
		t.Errorf("tool = %v", tool)
	}
}
