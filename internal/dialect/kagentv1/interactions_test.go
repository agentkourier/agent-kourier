package kagentv1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// fixtureTask returns the task in the last event of a recorded SSE stream under
// test/fixtures/kagent.
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

// pausedOn returns a task paused with the given HITL payload, as a payload not captured
// from kagent (a source-only shape, or a malformed one).
func pausedOn(payload map[string]any) a2a.Task {
	return a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{
		State:   a2a.TaskStateInputRequired,
		Message: &a2a.Message{Metadata: map[string]any{HITLExtension: payload}},
	}}
}

func TestParseInteraction(t *testing.T) {
	q := func(text string, multiple bool, choices ...string) dialect.Question {
		return dialect.Question{Text: text, Choices: choices, Multiple: multiple}
	}
	tool := dialect.Tool{ID: "adk-eaf5e6dc-d569-4784-91af-2420f1eb0c97", CallID: "call_1", Name: "add_numbers", Args: map[string]any{"a": 3.0, "b": 5.0}}
	fixtures := []struct { // captured from kagent 1.0.0-alpha5 (WP-02)
		name, file string
		want       *dialect.Interaction // nil: the task is not a usable pause
	}{
		{"single-select question", "askuser/02-ask.response.sse", &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-88073c89-3d00-4c34-96c5-d3e1867424af",
			Questions: []dialect.Question{q("Which database should we use?", false, "PostgreSQL", "MySQL")}}},
		{"multi-select and free-text questions", "askuser/21-pause-good-two-answers.response.sse", &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-3e9067a2-a7b2-4850-b36e-d6b0f91e4cad",
			Questions: []dialect.Question{q("Which databases?", true, "PostgreSQL", "MySQL"), q("Favorite color?", false)}}},
		{"tool approval", "approval/01-pause-approve.response.sse", &dialect.Interaction{Kind: dialect.ToolApproval, Tools: []dialect.Tool{tool}}},
		{"sub-agent ask-user is flat at alpha5", "nested/01-pause.response.sse", &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "adk-a80b11e7-3305-45cf-85b4-378644236931",
			Questions: []dialect.Question{q("Which region?", false, "east", "west")}}},
		{"paused without the extension header: no payload", "askuser/01-ask-without-extension-header.response.sse", nil},
		{"completed task", "askuser/04-good-answer.response.sse", nil},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			task := fixtureTask(t, tc.file)
			got, ok := (&Dialect{}).ParseInteraction(task)
			if tc.want == nil {
				if ok {
					t.Fatalf("parsed %+v, want none", got)
				}
				return
			}
			tc.want.TaskID, tc.want.ContextID = task.ID, task.ContextID
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v (ok=%v)\nwant %+v", got, ok, tc.want)
			}
		})
	}

	// Payloads not captured at alpha5. The nested shape is from kagent's source types
	// (go/api/a2a/hitl.go), not from a live run.
	inner := map[string]any{"id": "inner-1", "call_id": "c9", "name": "delete_file", "args": map[string]any{"path": "/x"}}
	nested := func(tools ...any) map[string]any {
		return map[string]any{"subagent_name": "k8s-agent", "task_id": "sub-task", "context_id": "sub-ctx", "tools": tools}
	}
	wantNested := &dialect.Nested{Subagent: "k8s-agent", TaskID: "sub-task", ContextID: "sub-ctx"}
	askReq := map[string]any{"type": "ask_user_request", "id": "r1", "questions": []any{map[string]any{"question": "Region?", "choices": []any{"east"}}}}
	sources := []struct {
		name    string
		payload map[string]any
		want    *dialect.Interaction
	}{
		{"nested ask-user", merge(askReq, "nested", nested()), &dialect.Interaction{
			Kind: dialect.AskUser, RequestID: "r1", Nested: wantNested, Questions: []dialect.Question{q("Region?", false, "east")}}},
		{"nested tool approval decides the inner tools", map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{"id": "outer"}}, "nested": nested(inner)},
			&dialect.Interaction{Kind: dialect.ToolApproval, Nested: wantNested, Tools: []dialect.Tool{{ID: "inner-1", CallID: "c9", Name: "delete_file", Args: map[string]any{"path": "/x"}}}}},
		{"several tools in one pause", map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{"id": "a", "name": "x"}, map[string]any{"id": "b", "name": "y"}}},
			&dialect.Interaction{Kind: dialect.ToolApproval, Tools: []dialect.Tool{{ID: "a", Name: "x"}, {ID: "b", Name: "y"}}}},
		{"unknown type", map[string]any{"type": "surprise"}, nil},
		{"ask-user without an id", merge(askReq, "id", ""), nil},
		{"ask-user without questions", merge(askReq, "questions", []any{}), nil},
		{"tool approval without tools", map[string]any{"type": "tool_approval_request", "tools": []any{}}, nil},
		{"tool without an id", map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{"name": "x"}}}, nil},
	}
	for _, tc := range sources {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (&Dialect{}).ParseInteraction(pausedOn(tc.payload))
			if tc.want == nil {
				if ok {
					t.Fatalf("parsed %+v, want none", got)
				}
				return
			}
			tc.want.TaskID, tc.want.ContextID = "task-1", "ctx-1"
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v (ok=%v)\nwant %+v", got, ok, tc.want)
			}
		})
	}

	working := pausedOn(askReq)
	working.Status.State = a2a.TaskStateWorking
	if _, ok := (&Dialect{}).ParseInteraction(working); ok {
		t.Error("a task that is not input-required parsed")
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
