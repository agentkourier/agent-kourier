package generic_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

func TestConnectPinsEndpointAndPreservesActorRouting(t *testing.T) {
	// The URL is used as written: kagent 0.9.x answers a POST to ".../agent" with a 307 to ".../agent/", and the
	// dialect follows no redirect, so an operator who writes the slash needs it sent. The card is under the
	// endpoint either way.
	for _, tc := range []struct{ name, suffix, post string }{
		{"no trailing slash", "/a2a", "/a2a"},
		{"trailing slash kept", "/a2a/", "/a2a/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ate-target-actor") != "default/sre" {
					t.Errorf("identity or routing header missing")
				}
				if r.Header.Get("A2A-Extensions") != "" {
					t.Error("generic client negotiated kagent HITL")
				}
				switch r.URL.Path {
				case "/a2a/.well-known/agent-card.json":
					card := a2a.AgentCard{Name: "sre", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true}, SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://card.invalid/a2a", a2a.TransportProtocolJSONRPC)}}
					_ = json.NewEncoder(w).Encode(card)
				case tc.post:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"}}}`))
				default:
					t.Errorf("unexpected path %s, want the card or %s", r.URL.Path, tc.post)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			d := generic.New(time.Second)
			defer d.CloseIdleConnections()
			backend := &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{URL: srv.URL + tc.suffix}}
			client, err := d.Connect(t.Context(), backend, v1alpha1.AgentRef{Name: "logical", Namespace: "logical"}, dialect.Credentials{Token: secret.New("token"), Headers: dialect.Headers{"ate-target-actor": []secret.Value{secret.New("default/sre")}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1"}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

// TestAttributeNamesThePlatform pins that the a2a dialect attributes a turn as kagent-v1 does: the prefix label is the
// platform's display name and the actor metadata carries its ID.
func TestAttributeNamesThePlatform(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	generic.New(time.Second).Attribute(msg, dialect.User{ID: "U1", Name: "Ada", Platform: dialect.Platform{ID: "slack", Name: "Slack"}})
	if got, want := msg.Parts[0].Text(), "[Slack: Ada (U1)]\nhi"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	actor, _ := msg.Metadata[kagentv1.ActorMetadataKey].(map[string]any)
	if actor["id"] != "U1" || actor["name"] != "Ada" || actor["platform"] != "slack" || len(actor) != 3 {
		t.Errorf("actor = %v, want id, name and platform", actor)
	}
}

func TestPlainQuestionValidationAndStableIdentity(t *testing.T) {
	d := generic.New(time.Second)
	defer d.CloseIdleConnections()
	now := time.Now()
	task := a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &now, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which service?"))}}
	in, err := d.ParseInteraction(task)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := d.BuildResponse(*in, dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"checkout"}}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.TaskID != "t1" || msg.ContextID != "c1" || msg.Parts[0].Text() != "checkout" {
		t.Fatalf("wrong answer: %+v", msg)
	}
	for _, answer := range []dialect.Answer{{}, {RequestID: in.RequestID, Answers: [][]string{{" "}}}, {RequestID: "stale", Answers: [][]string{{"checkout"}}}} {
		if _, err := d.BuildResponse(*in, answer); err == nil {
			t.Error("invalid answer accepted")
		}
	}
	later := now.Add(time.Second)
	task.Status.Timestamp = &later
	same, err := d.ParseInteraction(task)
	if err != nil || same.RequestID != in.RequestID {
		t.Fatal("snapshot timestamp changed the question identity")
	}
	task.Status.Message.ID = "next-question"
	next, err := d.ParseInteraction(task)
	if err != nil || next.RequestID == in.RequestID {
		t.Fatal("successive questions share an ID")
	}
	task.Status.Timestamp = nil
	if _, err := d.ParseInteraction(task); err != nil {
		t.Fatal("message ID did not correlate the question", err)
	}
	task.Status.Message.ID = ""
	if _, err := d.ParseInteraction(task); !errors.Is(err, dialect.ErrUnusablePause) {
		t.Fatal("accepted a question without a durable identity")
	}
	task.Status.Timestamp = &later
	if _, err := d.ParseInteraction(task); err != nil {
		t.Fatal("timestamp fallback failed", err)
	}
	task.Status.Message.Parts = append(task.Status.Message.Parts, a2a.NewDataPart(map[string]any{"approve": true}))
	if _, err := d.ParseInteraction(task); !errors.Is(err, dialect.ErrUnusablePause) {
		t.Fatalf("structured pause = %v", err)
	}
}

func TestRejectRequiredExtensionAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{true: "redirect", false: "required extension"}[redirect], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, "http://untrusted.invalid/card", http.StatusFound)
					return
				}
				card := a2a.AgentCard{Name: "sre", Version: "1", Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: "https://example.com/approval", Required: true}}}}
				_ = json.NewEncoder(w).Encode(card)
			}))
			defer srv.Close()
			d := generic.New(time.Second)
			defer d.CloseIdleConnections()
			_, err := d.Connect(t.Context(), &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{URL: srv.URL}}, v1alpha1.AgentRef{}, dialect.Credentials{UserID: "service"})
			if err == nil {
				t.Fatal("unsafe connection accepted")
			}
			if strings.Contains(err.Error(), "service") {
				t.Fatal("credential appears in error")
			}
		})
	}
}

// The generic dialect reads a pause as text and nothing else: even a task carrying kagent's
// HITL tool-approval metadata never becomes a tool approval, because Agent Kourier would then
// answer with a decision the backend does not understand. It is a text question, or unusable.
func TestNeverReturnsToolApproval(t *testing.T) {
	d := generic.New(time.Second)
	defer d.CloseIdleConnections()
	approval := map[string]any{
		"type":  "tool_approval_request",
		"hint":  "Please approve or reject the tool call add_numbers()",
		"tools": []any{map[string]any{"id": "adk-1", "call_id": "call_1", "name": "add_numbers", "args": map[string]any{"a": 3, "b": 5}}},
	}
	paused := func(msg *a2a.Message) a2a.Task {
		return a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}}
	}
	withMeta := func(msg *a2a.Message, meta map[string]any) *a2a.Message {
		msg.ID, msg.Metadata = "m1", meta
		return msg
	}
	hitl := map[string]any{kagentv1.HITLExtension: approval}
	for _, tc := range []struct {
		name string
		task a2a.Task
		text bool // a text question; otherwise ErrUnusablePause
	}{
		{"plain text question", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which service?")), nil)), true},
		{"HITL approval metadata with the hint as text", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Please approve or reject add_numbers()")), hitl)), true},
		{"HITL approval metadata with a data part", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewDataPart(approval)), hitl)), false},
		{"HITL approval metadata and no parts", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent), hitl)), false},
		{"nested HITL approval metadata", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Approve?")), map[string]any{kagentv1.HITLExtension: map[string]any{
			"type": "tool_approval_request", "tools": approval["tools"],
			"nested": map[string]any{"subagent_name": "k8s-agent", "task_id": "sub", "tools": approval["tools"]},
		}})), true},
		{"no message", paused(nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := d.ParseInteraction(tc.task)
			if !tc.text {
				if in != nil || !errors.Is(err, dialect.ErrUnusablePause) {
					t.Errorf("got %+v, %v; want ErrUnusablePause", in, err)
				}
				return
			}
			if err != nil || in == nil || in.Kind != dialect.AskUser || !in.TextReplies || len(in.Questions) != 1 || len(in.Tools) != 0 || in.Nested != nil {
				t.Errorf("got %+v, %v; want a text question with no tools", in, err)
			}
		})
	}
}
