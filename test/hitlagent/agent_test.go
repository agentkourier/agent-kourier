package hitlagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

const actorKey = "agentkourier.dev/actor"

type harness struct {
	srv    *httptest.Server
	client *a2aclient.Client
}

// newHarness serves the agent and builds an a2a-go client from its card, pointed at the test server's root as
// Agent Kourier would be (it ignores the card's interface URL and uses the configured endpoint).
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHookedHarness(t, hooks{})
}

// newHookedHarness is newHarness with test hooks in the agent.
func newHookedHarness(t *testing.T, hk hooks) *harness {
	t.Helper()
	h, err := newHandler("", hk)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx := testContext(t)
	card, err := agentcard.DefaultResolver.Resolve(ctx, srv.URL)
	if err != nil {
		t.Fatalf("resolve card: %v", err)
	}
	card.SupportedInterfaces = []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL+"/", a2a.TransportProtocolJSONRPC)}
	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = client.Destroy() })
	return &harness{srv: srv, client: client}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// withExtension asks for the HITL extension the way Agent Kourier does, with the A2A-Extensions header.
func withExtension(ctx context.Context) context.Context {
	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{a2a.SvcParamExtensions: {ExtensionURI}})
}

func (h *harness) stream(ctx context.Context, t *testing.T, msg *a2a.Message) ([]a2a.Event, error) {
	t.Helper()
	var events []a2a.Event
	for ev, err := range h.client.SendStreamingMessage(withExtension(ctx), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func userTurn(text string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("[Slack: Ada (U123) <ada@example.com>]\n"+text))
	msg.Metadata = map[string]any{actorKey: map[string]any{"id": "U123", "platform": "slack", "name": "Ada"}}
	return msg
}

// answer builds an approval answer as the agent contract describes Agent Kourier's.
func answer(task a2a.TaskInfo, payload map[string]any, text string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID = task.TaskID
	msg.ContextID = task.ContextID
	msg.Extensions = []string{ExtensionURI}
	msg.Metadata = map[string]any{
		ExtensionURI: payload,
		actorKey:     map[string]any{"id": "U123", "platform": "slack"},
	}
	return msg
}

func approvals(entries ...map[string]any) map[string]any {
	list := make([]any, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	return map[string]any{"type": "tool_approval_response", "approvals": list}
}

// pause sends the trigger turn and returns the final input-required status update and its decoded request.
func (h *harness) pause(ctx context.Context, t *testing.T) (*a2a.TaskStatusUpdateEvent, ToolApprovalRequest) {
	t.Helper()
	events, err := h.stream(ctx, t, userTurn("please use the approval tool"))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("stream sent no events")
	}
	last, ok := events[len(events)-1].(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("last event is %T, want *a2a.TaskStatusUpdateEvent", events[len(events)-1])
	}
	if last.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("last state = %s, want input-required", last.Status.State)
	}
	req, ok := pendingApproval(&a2a.Task{Status: last.Status})
	if !ok {
		t.Fatalf("status message carries no approval request: %+v", last.Status.Message)
	}
	return last, req
}

func finalArtifactText(t *testing.T, events []a2a.Event) string {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last, ok := events[len(events)-1].(*a2a.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("last event = %#v, want a completed status update", events[len(events)-1])
	}
	var texts []string
	for _, ev := range events {
		au, ok := ev.(*a2a.TaskArtifactUpdateEvent)
		if !ok {
			continue
		}
		for _, p := range au.Artifact.Parts {
			if p.Text() != "" {
				texts = append(texts, p.Text())
			}
		}
	}
	return strings.Join(texts, "")
}

func TestCardListsExtension(t *testing.T) {
	h := newHarness(t)
	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, h.srv.URL+"/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var card struct {
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
		Capabilities struct {
			Streaming  bool `json:"streaming"`
			Extensions []struct {
				URI      string `json:"uri"`
				Required *bool  `json:"required"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if !card.Capabilities.Streaming {
		t.Error("capabilities.streaming = false, want true")
	}
	idx := slices.IndexFunc(card.Capabilities.Extensions, func(e struct {
		URI      string `json:"uri"`
		Required *bool  `json:"required"`
	}) bool {
		return e.URI == ExtensionURI
	})
	if idx < 0 {
		t.Fatalf("capabilities.extensions = %+v, want %s", card.Capabilities.Extensions, ExtensionURI)
	}
	if r := card.Capabilities.Extensions[idx].Required; r != nil && *r {
		t.Error("extension is required, want optional")
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolBinding != string(a2a.TransportProtocolJSONRPC) ||
		card.SupportedInterfaces[0].URL != DefaultAgentURL || card.SupportedInterfaces[0].ProtocolVersion != string(a2a.Version) {
		t.Errorf("supportedInterfaces = %+v, want one JSON-RPC interface at %s", card.SupportedInterfaces, DefaultAgentURL)
	}
}

func TestPauseShape(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	ev, req := h.pause(ctx, t)

	if ev.TaskID == "" || ev.ContextID == "" {
		t.Fatalf("pause names task %q context %q, want both", ev.TaskID, ev.ContextID)
	}
	msg := ev.Status.Message
	if msg.Role != a2a.MessageRoleAgent {
		t.Errorf("status message role = %s, want agent", msg.Role)
	}
	if !slices.Contains(msg.Extensions, ExtensionURI) {
		t.Errorf("status message extensions = %v, want %s", msg.Extensions, ExtensionURI)
	}
	if len(msg.Parts) == 0 {
		t.Error("status message has no parts")
	}
	for _, p := range msg.Parts {
		if p.Text() == "" {
			t.Errorf("status message part %#v is not non-empty text", p)
		}
	}
	if req.Type != "tool_approval_request" || req.Hint == "" || len(req.Tools) != 1 {
		t.Fatalf("request = %+v, want one tool with a hint", req)
	}
	tool := req.Tools[0]
	if tool.ID == "" || tool.CallID == "" || tool.Name != "add_numbers" {
		t.Errorf("tool = %+v, want add_numbers with id and call_id", tool)
	}
	if tool.Args["a"] != float64(3) || tool.Args["b"] != float64(5) || len(tool.Args) != 2 {
		t.Errorf("args = %v, want {a: 3, b: 5}", tool.Args)
	}

	task, err := h.client.GetTask(ctx, &a2a.GetTaskRequest{ID: ev.TaskID})
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("GetTask state = %s, want input-required", task.Status.State)
	}
}

func TestApproveRunsTool(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	ev, req := h.pause(ctx, t)

	events, err := h.stream(ctx, t, answer(ev.TaskInfo(),
		approvals(map[string]any{"id": req.Tools[0].ID, "approved": true}), "approved"))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if got, want := finalArtifactText(t, events), "Approved: 3 + 5 = 8."; got != want {
		t.Errorf("artifact text = %q, want %q", got, want)
	}
}

func TestRepeatedAnswerIsNotRunAgain(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	ev, req := h.pause(ctx, t)
	msg := answer(ev.TaskInfo(), approvals(map[string]any{"id": req.Tools[0].ID, "approved": true}), "approved")

	if _, err := h.stream(ctx, t, msg); err != nil {
		t.Fatalf("answer: %v", err)
	}
	again, err := h.stream(ctx, t, msg)
	if err != nil {
		t.Fatalf("answer sent again: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("answer sent again gave %d events, want one task snapshot", len(again))
	}
	task, ok := again[0].(*a2a.Task)
	if !ok || task.ID != ev.TaskID || task.Status.State != a2a.TaskStateCompleted || len(task.Artifacts) != 1 {
		t.Errorf("answer sent again = %#v, want the completed task with one artifact", again[0])
	}
}

func TestDenyGivesReason(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	ev, req := h.pause(ctx, t)

	events, err := h.stream(ctx, t, answer(ev.TaskInfo(), approvals(map[string]any{
		"id": req.Tools[0].ID, "approved": false, "rejection_reason": "denied by an approver in chat",
	}), "rejected"))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if got, want := finalArtifactText(t, events), "Denied: denied by an approver in chat"; got != want {
		t.Errorf("artifact text = %q, want %q", got, want)
	}
}

func TestMalformedAnswerIsRefusedAndTaskStaysPaused(t *testing.T) {
	cases := []struct {
		name    string
		payload func(id string) map[string]any
	}{
		{"no payload", func(string) map[string]any { return nil }},
		{"wrong type", func(id string) map[string]any {
			p := approvals(map[string]any{"id": id, "approved": true})
			p["type"] = "ask_user_response"
			return p
		}},
		{"unknown tool id", func(string) map[string]any {
			return approvals(map[string]any{"id": "not-asked", "approved": true})
		}},
		{"tool not decided", func(string) map[string]any { return approvals() }},
		{"tool decided twice", func(id string) map[string]any {
			return approvals(map[string]any{"id": id, "approved": true}, map[string]any{"id": id, "approved": false})
		}},
		{"no approved flag", func(id string) map[string]any { return approvals(map[string]any{"id": id}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := testContext(t)
			ev, req := h.pause(ctx, t)

			msg := answer(ev.TaskInfo(), tc.payload(req.Tools[0].ID), "approved")
			if tc.payload(req.Tools[0].ID) == nil {
				delete(msg.Metadata, ExtensionURI)
			}
			code := rawStreamErrorCode(ctx, t, h.srv.URL+"/", msg)
			if code != -32603 {
				t.Errorf("JSON-RPC error code = %d, want -32603", code)
			}

			// The a2a-go client surfaces the same refusal as ErrInternalError.
			if _, err := h.stream(ctx, t, msg); !errors.Is(err, a2a.ErrInternalError) {
				t.Errorf("client error = %v, want ErrInternalError", err)
			}

			task, err := h.client.GetTask(ctx, &a2a.GetTaskRequest{ID: ev.TaskID})
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if task.Status.State != a2a.TaskStateInputRequired {
				t.Errorf("state after refusal = %s, want input-required", task.Status.State)
			}
		})
	}
}

// rawStreamErrorCode posts a SendStreamingMessage by hand and returns the JSON-RPC error code in the stream, so the
// test checks the wire and not only the client's mapping.
func rawStreamErrorCode(ctx context.Context, t *testing.T, endpoint string, msg *a2a.Message) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage",
		"params": a2a.SendMessageRequest{Message: msg},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(a2a.SvcParamVersion, string(a2a.Version))
	req.Header.Set(a2a.SvcParamExtensions, ExtensionURI)
	req.Header.Set("Authorization", "Bearer fake-test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get(a2a.SvcParamExtensions); got != ExtensionURI {
		t.Errorf("response A2A-Extensions = %q, want %q", got, ExtensionURI)
	}

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var frame struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", data, err)
		}
		if frame.Error != nil {
			return frame.Error.Code
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	t.Fatal("stream carried no JSON-RPC error")
	return 0
}

func TestOtherMessageGreets(t *testing.T) {
	h := newHarness(t)
	events, err := h.stream(testContext(t), t, userTurn("hello there"))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	got := finalArtifactText(t, events)
	if !strings.HasPrefix(got, "Hello from hitlagent.") || !strings.Contains(got, "hello there") || strings.Contains(got, "\n") {
		t.Errorf("greeting = %q, want one line echoing the message", got)
	}
}

func TestRepeatedMessageIDIsSameTurn(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	msg := userTurn("hello once")

	first, err := h.stream(ctx, t, msg)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	again, err := h.stream(ctx, t, msg)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	firstTask := first[0].TaskInfo().TaskID
	if len(again) != 1 {
		t.Fatalf("second send gave %d events, want one task snapshot", len(again))
	}
	task, ok := again[0].(*a2a.Task)
	if !ok || task.ID != firstTask || task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("second send = %#v, want the completed task %s", again[0], firstTask)
	}
}

func TestListTasksFindsTurnByMessageID(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	msg := userTurn("hello, list me")
	events, err := h.stream(ctx, t, msg)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info := events[0].TaskInfo()

	list, err := h.client.ListTasks(ctx, &a2a.ListTasksRequest{ContextID: info.ContextID})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID != info.TaskID {
		t.Fatalf("tasks = %+v, want the turn's task", list.Tasks)
	}
	if !slices.ContainsFunc(list.Tasks[0].History, func(m *a2a.Message) bool { return m.ID == msg.ID }) {
		t.Errorf("history %+v holds no message %s", list.Tasks[0].History, msg.ID)
	}
}

func TestCancelPausedTask(t *testing.T) {
	h := newHarness(t)
	ctx := testContext(t)
	ev, _ := h.pause(ctx, t)

	task, err := h.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: ev.TaskID})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("state = %s, want canceled", task.Status.State)
	}
}

// TestCancelAsPausedTurnEnds cancels a paused task while the SDK still holds the turn that paused it: the client has
// the input-required event, but Execute has not returned. The SDK hands such a cancel to the finished turn, which
// cannot take it, so the agent must cancel the task again once the turn is gone.
func TestCancelAsPausedTurnEnds(t *testing.T) {
	cancelSeen, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := newHookedHarness(t, hooks{
		paused: func() {
			select { // hold the turn until the cancel reaches the agent
			case <-cancelSeen:
			case <-done:
			}
		},
		canceling: func() { once.Do(func() { close(cancelSeen) }) },
	})
	t.Cleanup(func() { close(done) })
	ctx := testContext(t)
	ev, _ := h.pause(ctx, t)

	task, err := h.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: ev.TaskID})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("state = %s, want canceled", task.Status.State)
	}
}
