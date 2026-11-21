package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func newHandler(t *testing.T, fake *fakeBackend, keepalive time.Duration) a2asrv.RequestHandler {
	t.Helper()
	return NewRequestHandler(&Executor{Backend: fake, Store: newStore(t, nil), Keepalive: keepalive})
}

// turn sends msg and returns the task it settled in and the events the stream carried.
func turn(t *testing.T, h a2asrv.RequestHandler, msg *a2a.Message) (*a2a.Task, []a2a.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []a2a.Event
	var id a2a.TaskID
	for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		events = append(events, ev)
		id = ev.TaskInfo().TaskID
	}
	task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task, events
}

func userText(contextID, text string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ContextID = contextID
	return m
}

// script answers each user.message with the given events, after the echo and status_running.
func script(events ...Event) func(string, Outbound) []Event {
	return func(_ string, out Outbound) []Event {
		if out.Message == nil {
			return nil
		}
		return append([]Event{{Type: evUserMessage, ProcessedAt: processed()}, running()}, events...)
	}
}

func artifactText(task *a2a.Task) string {
	var b strings.Builder
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			if s, ok := p.Content.(a2a.Text); ok {
				b.WriteString(string(s))
			}
		}
	}
	return b.String()
}

func TestTextTurnCompletes(t *testing.T) {
	fake := newFake(script(say("Hello "), say("there."), idle(stopEndTurn)))
	task, _ := turn(t, newHandler(t, fake, 0), userText("", "hi"))

	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}
	if got := artifactText(task); got != "Hello there." {
		t.Errorf("artifact text = %q", got)
	}
	sent := fake.sentEvents()
	if len(sent) != 1 || sent[0].out.Message == nil || *sent[0].out.Message != "hi" {
		t.Fatalf("sent = %+v, want one user.message \"hi\"", sent)
	}
}

func TestContextReusesSession(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	h := newHandler(t, fake, 0)
	first, _ := turn(t, h, userText("", "one"))
	second, _ := turn(t, h, userText(first.ContextID, "two"))
	if second.ContextID != first.ContextID {
		t.Fatalf("context changed: %s then %s", first.ContextID, second.ContextID)
	}
	if n := fake.sessionCount(); n != 1 {
		t.Errorf("sessions created = %d, want 1", n)
	}
	other, _ := turn(t, h, userText("", "three"))
	if other.ContextID == first.ContextID {
		t.Fatal("a new context reused the old one")
	}
	if n := fake.sessionCount(); n != 2 {
		t.Errorf("sessions created = %d, want 2", n)
	}
}

func TestToolCallAndResultAreADKParts(t *testing.T) {
	// On a self-hosted environment the worker posts the result as user.tool_result; a cloud one sends agent.tool_result.
	for _, resultType := range []string{evAgentToolResult, evUserToolResult} {
		t.Run(resultType, func(t *testing.T) { testToolParts(t, resultType) })
	}
}

func testToolParts(t *testing.T, resultType string) {
	sent := toolResult("sevt_tool", "cma-sandbox-x")
	sent.Type = resultType
	fake := newFake(script(
		toolUse("sevt_tool", "bash", map[string]any{"command": "hostname"}, "allow"),
		sent,
		say("The host is cma-sandbox-x."),
		idle(stopEndTurn),
	))
	task, _ := turn(t, newHandler(t, fake, 0), userText("", "hostname?"))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	var call, result map[string]any
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			data, ok := p.Data().(map[string]any)
			if !ok {
				continue
			}
			switch p.Metadata["adk_type"] {
			case "function_call":
				call = data
			case "function_response":
				result = data
			}
		}
	}
	if call == nil || call["id"] != "sevt_tool" || call["name"] != "bash" {
		t.Fatalf("function_call part = %v", call)
	}
	if args, _ := call["args"].(map[string]any); args["command"] != "hostname" {
		t.Errorf("args = %v", call["args"])
	}
	if result == nil || result["id"] != "sevt_tool" || result["name"] != "bash" {
		t.Fatalf("function_response part = %v", result)
	}
	if resp, _ := result["response"].(map[string]any); resp["output"] != "cma-sandbox-x" {
		t.Errorf("response = %v", result["response"])
	}
	if !strings.Contains(artifactText(task), "cma-sandbox-x.") {
		t.Errorf("text = %q", artifactText(task))
	}
}

// approvalScript asks to run bash, then on a confirmation reports the outcome.
func approvalScript(_ string, out Outbound) []Event {
	switch {
	case out.Message != nil:
		return []Event{
			{Type: evUserMessage, ProcessedAt: processed()}, running(),
			say("I need to run a command."),
			toolUse("sevt_ask", "bash", map[string]any{"command": "rm -rf /tmp/x"}, permissionAsk),
			idle(stopRequiresAction, "sevt_ask"),
		}
	case out.Confirmation != nil && out.Confirmation.Allow:
		return []Event{running(), toolResult("sevt_ask", ""), say("Done."), idle(stopEndTurn)}
	case out.Confirmation != nil:
		return []Event{running(), say("Understood, I did not run it."), idle(stopEndTurn)}
	}
	return nil
}

func hitlRequest(t *testing.T, task *a2a.Task) map[string]any {
	t.Helper()
	msg := task.Status.Message
	if msg == nil {
		t.Fatal("paused task has no status message")
	}
	if len(msg.Extensions) != 1 || msg.Extensions[0] != HITLExtension {
		t.Fatalf("extensions = %v", msg.Extensions)
	}
	raw, err := json.Marshal(msg.Metadata[HITLExtension])
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func approvalReply(task *a2a.Task, approved bool, reason string) *a2a.Message {
	decision := map[string]any{"id": "sevt_ask", "approved": approved}
	if reason != "" {
		decision["rejection_reason"] = reason
	}
	text := "approved"
	if !approved {
		text = "rejected"
	}
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.TaskID, m.ContextID = task.ID, task.ContextID
	m.Extensions = []string{HITLExtension}
	m.Metadata = map[string]any{HITLExtension: map[string]any{"type": "tool_approval_response", "approvals": []any{decision}}}
	return m
}

func TestApprovalPausesAndAllowResumes(t *testing.T) {
	fake := newFake(approvalScript)
	h := newHandler(t, fake, 0)
	paused, _ := turn(t, h, userText("", "clean up"))
	if paused.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s, want input-required", paused.Status.State)
	}
	req := hitlRequest(t, paused)
	if req["type"] != "tool_approval_request" {
		t.Fatalf("type = %v", req["type"])
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", req["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["id"] != "sevt_ask" || tool["name"] != "bash" {
		t.Fatalf("tool = %v", tool)
	}
	if args, _ := tool["args"].(map[string]any); args["command"] != "rm -rf /tmp/x" {
		t.Errorf("args = %v", tool["args"])
	}

	done, _ := turn(t, h, approvalReply(paused, true, ""))
	if done.ID != paused.ID || done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("after allow: task %s state %s", done.ID, done.Status.State)
	}
	sent := fake.sentEvents()
	last := sent[len(sent)-1].out.Confirmation
	if last == nil || last.ToolUseID != "sevt_ask" || !last.Allow {
		t.Fatalf("confirmation = %+v", last)
	}
	if !strings.Contains(artifactText(done), "Done.") {
		t.Errorf("text = %q", artifactText(done))
	}
}

func TestRejectSendsDenyWithReason(t *testing.T) {
	fake := newFake(approvalScript)
	h := newHandler(t, fake, 0)
	paused, _ := turn(t, h, userText("", "clean up"))
	done, _ := turn(t, h, approvalReply(paused, false, "not on prod"))
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", done.Status.State)
	}
	sent := fake.sentEvents()
	c := sent[len(sent)-1].out.Confirmation
	if c == nil || c.Allow || c.DenyMessage != "not on prod" {
		t.Fatalf("confirmation = %+v", c)
	}
}

func TestPlainTextCannotAnswerApproval(t *testing.T) {
	fake := newFake(approvalScript)
	h := newHandler(t, fake, 0)
	paused, _ := turn(t, h, userText("", "clean up"))
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("yes"))
	m.TaskID, m.ContextID = paused.ID, paused.ContextID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var failed bool
	for _, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: m}) {
		if err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatal("plain text answered a tool approval")
	}
	for _, s := range fake.sentEvents() {
		if s.out.Confirmation != nil {
			t.Fatal("a confirmation was sent for plain text")
		}
	}
	still, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: paused.ID})
	if err != nil || still.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("after a refused answer: %v, state %v; want the task still paused", err, still.Status.State)
	}
	// The pause can still be answered.
	done, _ := turn(t, h, approvalReply(paused, true, ""))
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", done.Status.State)
	}
}

func TestFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		last Event
	}{
		{"retries exhausted", idle("retries_exhausted")},
		{"budget reached", idle("budget_reached")},
		{"terminated", Event{Type: evStatusTerminated}},
		{"custom tool", idle(stopRequiresAction, "sevt_custom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(script(say("partial"), tc.last))
			task, _ := turn(t, newHandler(t, fake, 0), userText("", "go"))
			if task.Status.State != a2a.TaskStateFailed {
				t.Fatalf("state = %s, want failed", task.Status.State)
			}
			if task.Status.Message == nil {
				t.Fatal("failed task says nothing")
			}
		})
	}
}

func TestSessionErrorDoesNotEndTurn(t *testing.T) {
	fake := newFake(script(Event{Type: evSessionError, Error: &EventError{Type: "model_overloaded", Message: "retrying"}}, say("recovered"), idle(stopEndTurn)))
	task, _ := turn(t, newHandler(t, fake, 0), userText("", "go"))
	if task.Status.State != a2a.TaskStateCompleted || artifactText(task) != "recovered" {
		t.Fatalf("state %s text %q", task.Status.State, artifactText(task))
	}
}

func TestIdleBeforeRunningIsIgnored(t *testing.T) {
	// A stale idle (the session was idle before the message) must not end the turn.
	fake := newFake(func(_ string, out Outbound) []Event {
		if out.Message == nil {
			return nil
		}
		return []Event{idle(stopEndTurn), {Type: evUserMessage, ProcessedAt: processed()}, running(), say("real answer"), idle(stopEndTurn)}
	})
	task, _ := turn(t, newHandler(t, fake, 0), userText("", "go"))
	if artifactText(task) != "real answer" {
		t.Fatalf("text = %q", artifactText(task))
	}
}

func TestKeepaliveDuringSilence(t *testing.T) {
	var fake *fakeBackend
	fake = newFake(func(session string, out Outbound) []Event {
		if out.Message == nil {
			return nil
		}
		go func() {
			time.Sleep(120 * time.Millisecond)
			fake.publish(session, say("late"), idle(stopEndTurn))
		}()
		return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running()}
	})
	_, events := turn(t, newHandler(t, fake, 20*time.Millisecond), userText("", "slow"))
	working := 0
	for _, ev := range events {
		if s, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && s.Status.State == a2a.TaskStateWorking {
			working++
		}
	}
	if working < 3 {
		t.Fatalf("working updates = %d, want keepalives during the silence", working)
	}
}

func TestDuplicateMessageIDJoinsTheTask(t *testing.T) {
	fake := newFake(script(say("once"), idle(stopEndTurn)))
	h := newHandler(t, fake, 0)
	msg := userText("", "hi")
	first, _ := turn(t, h, msg)
	again := *msg
	again.ContextID = first.ContextID
	second, _ := turn(t, h, &again)
	if second.ID != first.ID {
		t.Fatalf("resend made task %s, want %s", second.ID, first.ID)
	}
	if n := len(fake.sentEvents()); n != 1 {
		t.Fatalf("messages sent to the session = %d, want 1", n)
	}
}

func TestCancelInterrupts(t *testing.T) {
	fake := newFake(func(_ string, out Outbound) []Event {
		switch {
		case out.Message != nil:
			return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(), say("working on it")}
		case out.Interrupt:
			return []Event{idle(stopEndTurn)}
		}
		return nil
	})
	h := newHandler(t, fake, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id a2a.TaskID
	for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("", "long job")}) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			id = ev.TaskInfo().TaskID
			break
		}
	}
	task, err := h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: id})
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %s", task.Status.State)
	}
	var interrupted bool
	for _, s := range fake.sentEvents() {
		interrupted = interrupted || s.out.Interrupt
	}
	if !interrupted {
		t.Fatal("no user.interrupt sent")
	}
}
