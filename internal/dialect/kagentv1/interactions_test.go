package kagentv1

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/dialecttest"
)

// pausedOn returns a task paused with the given HITL payload, as a payload not captured
// from kagent (a source-only shape, or a malformed one).
func pausedOn(payload map[string]any) a2a.Task {
	return a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{
		State:   a2a.TaskStateInputRequired,
		Message: &a2a.Message{Metadata: map[string]any{HITLExtension: payload}},
	}}
}

// mustParse returns the usable pause in task, or fails the test.
func mustParse(t *testing.T, task a2a.Task) *dialect.Interaction {
	t.Helper()
	in, err := (&Dialect{}).ParseInteraction(task)
	if err != nil || in == nil {
		t.Fatalf("the pause did not parse: %+v, %v", in, err)
	}
	return in
}

func TestParseInteraction(t *testing.T) {
	q := func(text string, multiple bool, choices ...string) dialect.Question {
		return dialect.Question{Text: text, Choices: choices, Multiple: multiple}
	}
	tool := dialect.Tool{ID: "adk-eaf5e6dc-d569-4784-91af-2420f1eb0c97", CallID: "call_1", Name: "add_numbers", Args: map[string]any{"a": 3.0, "b": 5.0}}
	const approvalHint = "Please approve or reject the tool call add_numbers() by responding with a FunctionResponse with an expected ToolConfirmation payload. (add_numbers)"
	fixtures := []struct { // captured from kagent 1.0.0-alpha5 (WP-02)
		name, file string
		want       *dialect.Interaction // nil: not a usable pause
		reason     string               // when want is nil: "" is (nil, nil), else the error's text
	}{
		{"single-select question", "askuser/02-ask.response.sse", &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-88073c89-3d00-4c34-96c5-d3e1867424af",
			Questions: []dialect.Question{q("Which database should we use?", false, "PostgreSQL", "MySQL")}}, ""},
		{"multi-select and free-text questions", "askuser/21-pause-good-two-answers.response.sse", &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-3e9067a2-a7b2-4850-b36e-d6b0f91e4cad",
			Questions: []dialect.Question{q("Which databases?", true, "PostgreSQL", "MySQL"), {Text: "Favorite color?", Choices: []string{}}}}, ""}, // kagent sends "choices": [] for free text
		{"tool approval", "approval/01-pause-approve.response.sse", &dialect.Interaction{Kind: dialect.ToolApproval, Hint: approvalHint, Tools: []dialect.Tool{tool}}, ""},
		{"sub-agent ask-user is flat at alpha5", "nested/01-pause.response.sse", &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-a80b11e7-3305-45cf-85b4-378644236931",
			Questions: []dialect.Question{q("Which region?", false, "east", "west")}}, ""},
		{"ask-user paused without the extension header: no payload", "askuser/01-ask-without-extension-header.response.sse", nil, "no HITL payload"},
		{"approval paused without the extension header: no payload", "approval/13-ask-without-extension-header.response.sse", nil, "no HITL payload"},
		{"completed task", "askuser/04-good-answer.response.sse", nil, ""},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			task := dialecttest.KagentTask(t, tc.file)
			if tc.want != nil {
				tc.want.TaskID, tc.want.ContextID, tc.want.Extension = task.ID, task.ContextID, HITLExtension
			}
			checkParse(t, task, tc.want, tc.reason)
		})
	}

	// Payloads not captured at alpha5. The nested ones are built the way kagent
	// v1.0.0-alpha5's BuildHITLStatusMessage builds them (go/adk/pkg/a2a/hitl.go), not
	// taken from a live run: WP-02 could not capture a nested case.
	askReq := map[string]any{"type": "ask_user_request", "id": "r1", "questions": []any{map[string]any{"question": "Region?", "choices": []any{"east"}}}}
	wantNested := &dialect.Nested{Subagent: "k8s-agent", TaskID: "sub-task", ContextID: "sub-ctx"}
	wantInner := dialect.Tool{ID: "inner-1", CallID: "c9", Name: "delete_file", Args: map[string]any{"path": "/x"}}
	sources := []struct {
		name    string
		payload map[string]any
		want    *dialect.Interaction // nil: not a usable pause
		reason  string               // when want is nil: the error's text
	}{
		{"nested ask-user is answered by the child tool's id", adkNestedAskUser(), &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-child-ask", Nested: wantNested, Questions: []dialect.Question{q("Region?", false, "east")}}, ""},
		{"nested tool approval decides the inner tools", adkNestedApproval(), &dialect.Interaction{
			Kind: dialect.ToolApproval, Hint: "Approval is required for tool(s): delete_file", Nested: wantNested, Tools: []dialect.Tool{wantInner}}, ""},
		{"nested ask-user without a subagent task", withNested(adkNestedAskUser(), "task_id", ""), nil, "nested request has no sub-agent name or task"},
		{"nested ask-user without a subagent name", withNested(adkNestedAskUser(), "subagent_name", ""), nil, "nested request has no sub-agent name or task"},
		{"nested ask-user without a child tool", withNested(adkNestedAskUser(), "tools", []any{}), nil, "exactly one ask_user tool"},
		{"nested ask-user with two child tools", withNested(adkNestedAskUser(), "tools", []any{
			map[string]any{"id": "a", "name": "ask_user"}, map[string]any{"id": "b", "name": "ask_user"}}), nil, "exactly one ask_user tool"},
		{"nested ask-user whose child tool is not ask_user", withNested(adkNestedAskUser(), "tools", []any{map[string]any{"id": "a", "name": "delete_file"}}), nil, "exactly one ask_user tool"},
		{"nested ask-user whose child tool has no id", withNested(adkNestedAskUser(), "tools", []any{map[string]any{"name": "ask_user"}}), nil, "tool has no id"},
		{"nested ask-user without a parent id", merge(adkNestedAskUser(), "id", ""), nil, "ask-user request has no id"},
		{"nested ask-user with an empty nested object", merge(adkNestedAskUser(), "nested", map[string]any{}), nil, "nested request has no sub-agent name or task"},
		{"nested approval without inner tools does not fall back to the outer ones", withNested(adkNestedApproval(), "tools", []any{}), nil, "no tools"},
		{"nested approval without a subagent task", withNested(adkNestedApproval(), "task_id", ""), nil, "nested request has no sub-agent name or task"},
		{"nested approval without a subagent name", withNested(adkNestedApproval(), "subagent_name", ""), nil, "nested request has no sub-agent name or task"},
		{"nested approval with an inner tool without an id", withNested(adkNestedApproval(), "tools", []any{map[string]any{"name": "x"}}), nil, "tool without an id"},
		{"nested approval with two outer tools", merge(adkNestedApproval(), "tools", []any{map[string]any{"id": "p1"}, map[string]any{"id": "p2"}}), nil, "exactly one outer tool"},
		{"nested approval with no outer tool", merge(adkNestedApproval(), "tools", []any{}), nil, "exactly one outer tool"},
		{"nested approval whose outer tool has no id", merge(adkNestedApproval(), "tools", []any{map[string]any{"name": "k8s-agent"}}), nil, "exactly one outer tool"},
		{"several tools in one pause", map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{"id": "a", "name": "x"}, map[string]any{"id": "b", "name": "y"}}},
			&dialect.Interaction{Kind: dialect.ToolApproval, Tools: []dialect.Tool{{ID: "a", Name: "x"}, {ID: "b", Name: "y"}}}, ""},
		{"unknown type", map[string]any{"type": "surprise"}, nil, "unknown request type"},
		{"a payload of the wrong shape", map[string]any{"type": 7}, nil, "malformed"},
		{"ask-user without an id", merge(askReq, "id", ""), nil, "ask-user request has no id"},
		{"ask-user without questions", merge(askReq, "questions", []any{}), nil, "no questions"},
		{"tool approval without tools", map[string]any{"type": "tool_approval_request", "tools": []any{}}, nil, "no tools"},
		{"tool without an id", map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{"name": "x"}}}, nil, "tool without an id"},
	}
	for _, tc := range sources {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want != nil {
				tc.want.TaskID, tc.want.ContextID, tc.want.Extension = "task-1", "ctx-1", HITLExtension
			}
			checkParse(t, pausedOn(tc.payload), tc.want, tc.reason)
		})
	}

	working := pausedOn(askReq)
	working.Status.State = a2a.TaskStateWorking
	checkParse(t, working, nil, "") // not paused: nothing to parse, and not an error
	noMessage := pausedOn(askReq)
	noMessage.Status.Message = nil
	checkParse(t, noMessage, nil, "no HITL payload")
	noID := pausedOn(askReq) // an answer is addressed to the task, so a pause on one with no ID could never be answered
	noID.ID = ""
	checkParse(t, noID, nil, "paused task has no ID")
	notAnObject := pausedOn(askReq)
	notAnObject.Status.Message.Metadata[HITLExtension] = "please answer"
	checkParse(t, notAnObject, nil, "malformed")
}

// The caps keep a hostile or broken agent from making Agent Kourier render an unbounded prompt:
// at the limit a pause parses, past it the pause is unusable.
func TestParseInteractionCaps(t *testing.T) {
	texts := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = fmt.Sprintf("c%d", i)
		}
		return out
	}
	questions := func(n int, choices int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"question": fmt.Sprintf("q%d", i), "choices": texts(choices)}
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		questions []any
		reason    string // "": usable
	}{
		{"20 questions", questions(20, 1), ""},
		{"21 questions", questions(21, 1), "more than 20 questions"},
		{"100 choices", questions(1, 100), ""},
		{"101 choices", questions(1, 101), "more than 100 choices"},
		{"101 choices on the second question", append(questions(1, 2), questions(1, 101)...), "more than 100 choices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := pausedOn(map[string]any{"type": "ask_user_request", "id": "r1", "questions": tc.questions})
			in, err := (&Dialect{}).ParseInteraction(task)
			if tc.reason == "" {
				if err != nil || in == nil || len(in.Questions) != len(tc.questions) {
					t.Errorf("got %+v, %v; want a usable pause", in, err)
				}
				return
			}
			if in != nil || !errors.Is(err, dialect.ErrUnusablePause) || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("got %+v, %v; want ErrUnusablePause mentioning %q", in, err, tc.reason)
			}
		})
	}
}

// A tool-approval pause is capped too, plain and nested, counted on the tools to decide: the
// nested tools for a nested request, not the one outer tool. The error is fixed text, so
// nothing the agent sent reaches a thread.
func TestParseInteractionToolCap(t *testing.T) {
	const hostile = "HOSTILE-TOOL-NAME"
	tools := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"id": fmt.Sprintf("t%d", i), "call_id": fmt.Sprintf("c%d", i), "name": hostile, "args": map[string]any{}}
		}
		return out
	}
	plain := func(n int) map[string]any {
		return map[string]any{"type": "tool_approval_request", "tools": tools(n)}
	}
	nested := func(n int) map[string]any {
		return withNested(adkNestedApproval(), "tools", tools(n))
	}
	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    int // tools in the interaction; 0: over the cap; -1: fails the one-outer-tool check first
	}{
		{"20 tools", plain(20), 20},
		{"21 tools", plain(21), 0},
		{"20 nested tools", nested(20), 20},
		{"21 nested tools", nested(21), 0},
		{"21 outer tools on a nested request", merge(nested(1), "tools", tools(21)), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := (&Dialect{}).ParseInteraction(pausedOn(tc.payload))
			if tc.want > 0 {
				if err != nil || in == nil || in.Kind != dialect.ToolApproval || len(in.Tools) != tc.want {
					t.Errorf("got %+v, %v; want a usable pause of %d tools", in, err, tc.want)
				}
				return
			}
			reason := "tool approval has more than 20 tools"
			if tc.want < 0 {
				reason = "nested tool approval needs exactly one outer tool with an id"
			}
			if in != nil || !errors.Is(err, dialect.ErrUnusablePause) || !strings.Contains(err.Error(), reason) {
				t.Errorf("got %+v, %v; want ErrUnusablePause mentioning %q", in, err, reason)
			}
			if err != nil && strings.Contains(err.Error(), hostile) {
				t.Errorf("the error carries agent text: %v", err)
			}
		})
	}
}

// checkParse parses task. A non-nil want is the usable pause it must give. Otherwise
// reason "" means (nil, nil): no structured pause. Any other reason means an error that
// wraps dialect.ErrUnusablePause and names that reason.
func checkParse(t *testing.T, task a2a.Task, want *dialect.Interaction, reason string) {
	t.Helper()
	got, err := (&Dialect{}).ParseInteraction(task)
	switch {
	case want != nil:
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, %v\nwant %+v", got, err, want)
		}
	case reason == "":
		if got != nil || err != nil {
			t.Errorf("got %+v, %v; want nil, nil", got, err)
		}
	default:
		if got != nil || !errors.Is(err, dialect.ErrUnusablePause) || !strings.Contains(err.Error(), reason) {
			t.Errorf("got %+v, %v; want ErrUnusablePause mentioning %q", got, err, reason)
		}
	}
}

// withNested returns a copy of an ADK nested payload with key of its nested object set to v.
func withNested(payload map[string]any, key string, v any) map[string]any {
	return merge(payload, "nested", merge(payload["nested"].(map[string]any), key, v))
}

// adkNestedAskUser is a sub-agent's ask-user pause as kagent v1.0.0-alpha5's
// BuildHITLStatusMessage builds it (go/adk/pkg/a2a/hitl.go): the top-level id is the
// parent's confirmation call, and the child ask_user tool carries the id the answer echoes.
func adkNestedAskUser() map[string]any {
	questions := []any{map[string]any{"question": "Region?", "choices": []any{"east"}, "multiple": false}}
	return map[string]any{
		"type": "ask_user_request", "id": "adk-parent-ask", "questions": questions,
		"nested": map[string]any{
			"subagent_name": "k8s-agent", "task_id": "sub-task", "context_id": "sub-ctx",
			"tools": []any{map[string]any{"id": "adk-child-ask", "call_id": "adk-child-ask", "name": "ask_user",
				"args": map[string]any{"questions": questions}}},
		},
	}
}

// adkNestedApproval is a sub-agent's tool-approval pause as BuildHITLStatusMessage builds
// it at alpha5: one outer tool (the parent's confirmation of the sub-agent call) and the
// inner tools to decide.
func adkNestedApproval() map[string]any {
	return map[string]any{
		"type": "tool_approval_request", "hint": "Approval is required for tool(s): delete_file",
		"tools": []any{map[string]any{"id": "adk-parent-tool", "call_id": "call-parent", "name": "k8s-agent", "args": map[string]any{}}},
		"nested": map[string]any{
			"subagent_name": "k8s-agent", "task_id": "sub-task", "context_id": "sub-ctx",
			"tools": []any{map[string]any{"id": "inner-1", "call_id": "c9", "name": "delete_file", "args": map[string]any{"path": "/x"}}},
		},
	}
}

// merge returns a copy of m with key set to v.
func merge(m map[string]any, key string, v any) map[string]any {
	out := map[string]any{key: v}
	for k, x := range m {
		if k != key {
			out[k] = x
		}
	}
	return out
}
