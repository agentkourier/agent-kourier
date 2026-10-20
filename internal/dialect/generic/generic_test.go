package generic_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// TestConnectNegotiatesHITLWhenTheCardListsIt pins that the a2a dialect asks for kagent's HITL extension on every call
// to an agent whose card lists it, and only then (TestConnectPinsEndpointAndPreservesActorRouting has the card without
// it). It is how a kagent agent sends its structured pause; without the header it sends ADK's hint as text alone.
func TestConnectNegotiatesHITLWhenTheCardListsIt(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "required"}[required], func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/a2a/.well-known/agent-card.json" {
					card := a2a.AgentCard{Name: "sre", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true, Extensions: []a2a.AgentExtension{{URI: kagentv1.HITLExtension, Required: required}}}, SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://card.invalid/a2a", a2a.TransportProtocolJSONRPC)}}
					_ = json.NewEncoder(w).Encode(card)
					return
				}
				posts.Add(1)
				if got := r.Header.Get("A2A-Extensions"); got != kagentv1.HITLExtension {
					t.Errorf("A2A-Extensions = %q, want %q", got, kagentv1.HITLExtension)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"}}}`))
			}))
			defer srv.Close()
			d := generic.New(time.Second)
			defer d.CloseIdleConnections()
			client, err := d.Connect(t.Context(), &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{URL: srv.URL + "/a2a"}}, v1alpha1.AgentRef{}, dialect.Credentials{Token: secret.New("token")})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1"}); err != nil {
				t.Fatal(err)
			}
			if posts.Load() != 1 {
				t.Fatalf("posts = %d, want 1", posts.Load())
			}
		})
	}
}

// fixtureTask returns the task in the last event of a recorded kagent SSE stream under test/fixtures/kagent.
func fixtureTask(t *testing.T, name string) a2a.Task {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "kagent", name))
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for line := range strings.SplitSeq(string(b), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			last = data
		}
	}
	var event struct {
		Result struct {
			Task a2a.Task `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(last), &event); err != nil {
		t.Fatal(err)
	}
	return event.Result.Task
}

// TestReadsHITLWhenTheMessageDeclaresIt pins which pauses the a2a dialect reads as kagent's HITL extension: a status
// message that lists the extension and carries its payload, as kagent sends it when the request asked for the
// extension. It is then read exactly as kagent-v1 reads it. Every other pause is a text question, or unusable, as it
// was before: a payload the message does not declare is not trusted to be the extension's.
func TestReadsHITLWhenTheMessageDeclaresIt(t *testing.T) {
	d := generic.New(time.Second)
	defer d.CloseIdleConnections()
	kagent := kagentv1.New(time.Second)
	defer kagent.CloseIdleConnections()
	approval := map[string]any{
		"type":  "tool_approval_request",
		"hint":  "Please approve or reject the tool call add_numbers()",
		"tools": []any{map[string]any{"id": "adk-1", "call_id": "call_1", "name": "add_numbers", "args": map[string]any{"a": 3, "b": 5}}},
	}
	paused := func(msg *a2a.Message) a2a.Task {
		return a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}}
	}
	withMeta := func(msg *a2a.Message, meta map[string]any, extensions ...string) *a2a.Message {
		msg.ID, msg.Metadata, msg.Extensions = "m1", meta, extensions
		return msg
	}
	hitl := map[string]any{kagentv1.HITLExtension: approval}
	for _, tc := range []struct {
		name string
		task a2a.Task
		want string // "text", "hitl" (as kagent-v1 reads it) or "unusable"
	}{
		{"plain text question", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which service?")), nil)), "text"},
		{"undeclared HITL metadata with the hint as text", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Please approve or reject add_numbers()")), hitl)), "text"},
		{"undeclared HITL metadata with a data part", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewDataPart(approval)), hitl)), "unusable"},
		{"declared extension without its payload", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which service?")), nil, kagentv1.HITLExtension)), "text"},
		{"declared HITL approval", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Approve?")), hitl, kagentv1.HITLExtension)), "hitl"},
		{"declared nested HITL approval", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Approve?")), map[string]any{kagentv1.HITLExtension: map[string]any{
			"type": "tool_approval_request", "tools": approval["tools"],
			"nested": map[string]any{"subagent_name": "k8s-agent", "task_id": "sub", "tools": approval["tools"]},
		}}, kagentv1.HITLExtension)), "hitl"},
		{"declared malformed HITL payload", paused(withMeta(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Approve?")), map[string]any{kagentv1.HITLExtension: map[string]any{"type": "tool_approval_request"}}, kagentv1.HITLExtension)), "unusable"},
		{"kagent tool approval, recorded", fixtureTask(t, "approval/01-pause-approve.response.sse"), "hitl"},
		{"kagent ask-user, recorded", fixtureTask(t, "askuser/02-ask.response.sse"), "hitl"},
		{"kagent tool approval without the header, recorded", fixtureTask(t, "approval/13-ask-without-extension-header.response.sse"), "text"},
		{"no message", paused(nil), "unusable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := d.ParseInteraction(tc.task)
			switch tc.want {
			case "unusable":
				if in != nil || !errors.Is(err, dialect.ErrUnusablePause) {
					t.Errorf("got %+v, %v; want ErrUnusablePause", in, err)
				}
			case "text":
				if err != nil || in == nil || in.Kind != dialect.AskUser || !in.TextReplies || len(in.Questions) != 1 || len(in.Tools) != 0 || in.Nested != nil {
					t.Errorf("got %+v, %v; want a text question with no tools", in, err)
				}
			case "hitl":
				want, werr := kagent.ParseInteraction(tc.task)
				if werr != nil || err != nil || !reflect.DeepEqual(in, want) || in.TextReplies {
					t.Errorf("got %+v, %v; want kagent-v1's %+v, %v, not taking text replies", in, err, want, werr)
				}
			}
		})
	}
}

// TestAnswersHITLAsKagentDoes pins that an answer to a HITL pause is kagent's structured reply, the one recorded as
// approving the call (approval/02), and that an answer to a text question stays plain text.
func TestAnswersHITLAsKagentDoes(t *testing.T) {
	d := generic.New(time.Second)
	defer d.CloseIdleConnections()
	in, err := d.ParseInteraction(fixtureTask(t, "approval/01-pause-approve.response.sse"))
	if err != nil || in.Kind != dialect.ToolApproval {
		t.Fatalf("pause = %+v, %v; want a tool approval", in, err)
	}
	msg, err := d.BuildResponse(*in, dialect.Answer{Decisions: []dialect.Decision{{ToolID: in.Tools[0].ID, Approved: true}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "kagent", "approval", "02-approve.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Params struct {
			Message struct {
				Extensions []string       `json:"extensions"`
				Metadata   map[string]any `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &recorded); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(msg.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(recorded.Params.Message.Metadata)
	if string(got) != string(want) || !reflect.DeepEqual(msg.Extensions, recorded.Params.Message.Extensions) || msg.TaskID != in.TaskID || msg.ContextID != in.ContextID {
		t.Errorf("answer = %+v\nmetadata %s\nwant %s with extensions %v", msg, got, want, recorded.Params.Message.Extensions)
	}
	// Attribution keeps the structured answer's one measured shape: the actor goes in metadata, not the text.
	d.Attribute(&msg, dialect.User{ID: "U1", Platform: dialect.Platform{ID: "slack", Name: "Slack"}})
	if msg.Parts[0].Text() != "approved" {
		t.Errorf("attributed answer text = %q, want it unprefixed", msg.Parts[0].Text())
	}
	if _, err := d.BuildResponse(*in, dialect.Answer{Answers: [][]string{{"yes"}}}); err == nil {
		t.Error("a typed answer decided a tool approval")
	}
}

// TestAnswersHITLQuestionsAsKagentDoes pins that the answer to a HITL question with choices is kagent's structured reply,
// the one recorded as answering it (askuser/04), and that an answer outside the choices is refused as kagent-v1 refuses it.
func TestAnswersHITLQuestionsAsKagentDoes(t *testing.T) {
	d := generic.New(time.Second)
	defer d.CloseIdleConnections()
	in, err := d.ParseInteraction(fixtureTask(t, "askuser/03-pause-good-answer.response.sse"))
	if err != nil || in.Kind != dialect.AskUser || in.TextReplies || len(in.Questions) != 1 || len(in.Questions[0].Choices) != 2 {
		t.Fatalf("pause = %+v, %v; want a HITL question with two choices", in, err)
	}
	msg, err := d.BuildResponse(*in, dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"PostgreSQL"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "kagent", "askuser", "04-good-answer.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Params struct {
			Message struct {
				Extensions []string       `json:"extensions"`
				Metadata   map[string]any `json:"metadata"`
				Parts      []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &recorded); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(msg.Metadata)
	want, _ := json.Marshal(recorded.Params.Message.Metadata)
	if string(got) != string(want) || !reflect.DeepEqual(msg.Extensions, recorded.Params.Message.Extensions) || msg.Parts[0].Text() != recorded.Params.Message.Parts[0].Text {
		t.Errorf("answer metadata %s, extensions %v, text %q; want the recorded %s, %v, %q", got, msg.Extensions, msg.Parts[0].Text(), want, recorded.Params.Message.Extensions, recorded.Params.Message.Parts[0].Text)
	}
	if _, err := d.BuildResponse(*in, dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"SQLite"}}}); err == nil {
		t.Error("an answer outside the choices was built")
	}
}
