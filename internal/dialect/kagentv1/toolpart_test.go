package kagentv1

import (
	"reflect"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

func dataPart(partType any, data any) *a2a.Part {
	p := a2a.NewDataPart(data)
	if partType != nil {
		p.Metadata = map[string]any{"kagent.dev/a2a/part-type": partType}
	}
	return p
}

func TestToolPart(t *testing.T) {
	call := map[string]any{"id": "call_1", "name": "add_numbers", "args": map[string]any{"a": 3.0}}
	result := map[string]any{"id": "call_1", "name": "add_numbers", "response": map[string]any{"output": "8"}}
	for _, tc := range []struct {
		name   string
		part   *a2a.Part
		want   dialect.ToolPart
		wantOK bool
	}{
		{"call", dataPart("function_call", call), dialect.ToolPart{Kind: dialect.ToolCall, ID: "call_1", Name: "add_numbers", Args: map[string]any{"a": 3.0}}, true},
		{"result", dataPart("function_response", result), dialect.ToolPart{Kind: dialect.ToolResult, ID: "call_1", Name: "add_numbers", Result: map[string]any{"output": "8"}}, true},
		{"unknown part type", dataPart("thought", call), dialect.ToolPart{}, false},
		{"part type is not a string", dataPart(7, call), dialect.ToolPart{}, false},
		{"no part type", dataPart(nil, call), dialect.ToolPart{}, false},
		{"data is not an object", dataPart("function_call", "text"), dialect.ToolPart{}, false},
		{"text part", a2a.NewTextPart("hello"), dialect.ToolPart{}, false},
		{"nil part", nil, dialect.ToolPart{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (&Dialect{}).ToolPart(tc.part)
			if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ToolPart = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The parts kagent alpha5 sent for a tool approval (WP-02): one call and one result.
func TestToolPartOnRecordedArtifacts(t *testing.T) {
	task := fixtureTask(t, "approval/01-pause-approve.response.sse")
	var kinds []dialect.ToolPartKind
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			if tp, ok := (&Dialect{}).ToolPart(p); ok {
				kinds = append(kinds, tp.Kind)
				if tp.ID != "call_1" || tp.Name != "add_numbers" {
					t.Errorf("part = %+v, want call_1 add_numbers", tp)
				}
			}
		}
	}
	if !reflect.DeepEqual(kinds, []dialect.ToolPartKind{dialect.ToolCall, dialect.ToolResult}) {
		t.Errorf("kinds = %v, want a call then a result", kinds)
	}
}
