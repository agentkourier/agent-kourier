package dialect_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// marked is a data part whose metadata is meta.
func marked(meta map[string]any, data any) *a2a.Part {
	p := a2a.NewDataPart(data)
	p.Metadata = meta
	return p
}

func TestDecodeToolPart(t *testing.T) {
	call := map[string]any{"id": "call_1", "name": "add_numbers", "args": map[string]any{"a": 3.0}}
	result := map[string]any{"id": "call_1", "name": "add_numbers", "response": map[string]any{"output": "8"}}
	wantCall := dialect.ToolPart{Kind: dialect.ToolCall, ID: "call_1", Name: "add_numbers", Args: map[string]any{"a": 3.0}}
	wantResult := dialect.ToolPart{Kind: dialect.ToolResult, ID: "call_1", Name: "add_numbers", Result: map[string]any{"output": "8"}}
	for _, tc := range []struct {
		name   string
		part   *a2a.Part
		want   dialect.ToolPart
		wantOK bool
	}{
		{"kagent 1.0 call", marked(map[string]any{"kagent.dev/a2a/part-type": "function_call"}, call), wantCall, true},
		{"kagent 1.0 result", marked(map[string]any{"kagent.dev/a2a/part-type": "function_response"}, result), wantResult, true},
		{"kagent 0.9.x call", marked(map[string]any{"kagent_type": "function_call"}, call), wantCall, true},
		{"kagent 0.9.x result", marked(map[string]any{"kagent_type": "function_response"}, result), wantResult, true},
		{"ADK call", marked(map[string]any{"adk_type": "function_call"}, call), wantCall, true},
		{"ADK result", marked(map[string]any{"adk_type": "function_response"}, result), wantResult, true},
		{"ADK long-running flag beside the marker", marked(map[string]any{"adk_type": "function_call", "adk_is_long_running": true}, call), wantCall, true},
		{"a call with no ID", marked(map[string]any{"adk_type": "function_call"}, map[string]any{"name": "ls"}), dialect.ToolPart{Kind: dialect.ToolCall, Name: "ls"}, true},

		{"ADK tool_call stays opaque", marked(map[string]any{"adk_type": "tool_call"}, call), dialect.ToolPart{}, false},
		{"ADK tool_response stays opaque", marked(map[string]any{"adk_type": "tool_response"}, result), dialect.ToolPart{}, false},
		{"ADK executable_code stays opaque", marked(map[string]any{"adk_type": "executable_code"}, call), dialect.ToolPart{}, false},
		{"ADK code_execution_result stays opaque", marked(map[string]any{"adk_type": "code_execution_result"}, result), dialect.ToolPart{}, false},
		{"an unknown type", marked(map[string]any{"kagent_type": "thought"}, call), dialect.ToolPart{}, false},
		{"a marker that is not a string", marked(map[string]any{"adk_type": 7}, call), dialect.ToolPart{}, false},
		{"no marker", marked(nil, call), dialect.ToolPart{}, false},
		{"a marker under another key", marked(map[string]any{"type": "function_call"}, call), dialect.ToolPart{}, false},
		{"data is not an object", marked(map[string]any{"adk_type": "function_call"}, "text"), dialect.ToolPart{}, false},
		{"no name", marked(map[string]any{"adk_type": "function_call"}, map[string]any{"id": "call_1"}), dialect.ToolPart{}, false},
		{"an empty name", marked(map[string]any{"adk_type": "function_response"}, map[string]any{"id": "call_1", "name": ""}), dialect.ToolPart{}, false},
		{"a name that is not a string", marked(map[string]any{"adk_type": "function_call"}, map[string]any{"name": 7}), dialect.ToolPart{}, false},
		{"a text part", a2a.NewTextPart("hello"), dialect.ToolPart{}, false},
		{"a nil part", nil, dialect.ToolPart{}, false},

		// Exactly one marker key with a usable value decides (spec D45).
		{"a null key does not mask the one that holds the marker", marked(map[string]any{"adk_type": nil, "kagent_type": "function_call"}, call), wantCall, true},
		{"a null key beside a null key", marked(map[string]any{"adk_type": nil, "kagent_type": nil}, call), dialect.ToolPart{}, false},
		{"a key that is not a marker does not mask the one that is", marked(map[string]any{"adk_type": "thought", "kagent_type": "function_response"}, result), wantResult, true},
		{"two keys that agree are ambiguous", marked(map[string]any{"adk_type": "function_call", "kagent_type": "function_call"}, call), dialect.ToolPart{}, false},
		{"two keys that disagree are ambiguous", marked(map[string]any{"adk_type": "function_call", "kagent.dev/a2a/part-type": "function_response"}, call), dialect.ToolPart{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dialect.DecodeToolPart(tc.part)
			if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecodeToolPart = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// fixtureLines returns the JSON of each event of a recorded SSE stream.
func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "kagent", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			lines = append(lines, []byte(data))
		}
	}
	return lines
}

// v10Parts returns the parts of the artifact updates of a stream recorded as A2A 1.0.
func v10Parts(t *testing.T, name string) []*a2a.Part {
	t.Helper()
	var parts []*a2a.Part
	for _, line := range fixtureLines(t, name) {
		var env struct {
			Result a2a.StreamResponse `json:"result"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if e, ok := env.Result.Event.(*a2a.TaskArtifactUpdateEvent); ok {
			parts = append(parts, e.Artifact.Parts...)
		}
	}
	return parts
}

// v03Parts returns the data parts of the status messages of a stream recorded as A2A 0.3, which the 1.0 types do
// not read: a part there is {kind, data, metadata}.
func v03Parts(t *testing.T, name string) []*a2a.Part {
	t.Helper()
	var parts []*a2a.Part
	for _, line := range fixtureLines(t, name) {
		var env struct {
			Result struct {
				Status struct {
					Message struct {
						Parts []struct {
							Kind     string         `json:"kind"`
							Data     any            `json:"data"`
							Metadata map[string]any `json:"metadata"`
						} `json:"parts"`
					} `json:"message"`
				} `json:"status"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, p := range env.Result.Status.Message.Parts {
			if p.Kind == "data" {
				parts = append(parts, marked(p.Metadata, p.Data))
			}
		}
	}
	return parts
}

// The recorded streams: kagent 1.0 marks tool parts in artifacts (alpha5), kagent 0.9.4 in status messages.
func TestDecodeToolPartOnRecordedStreams(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		parts   func(*testing.T, string) []*a2a.Part
		want    []string
	}{
		{"approval/01-pause-approve.response.sse", v10Parts, []string{"call call_1 add_numbers", "result call_1 add_numbers"}},
		{"v09-tools/01-tool-turn.response.sse", v03Parts, []string{"call k8s_get_resources_0_1e73ff4e k8s_get_resources", "result k8s_get_resources_0_1e73ff4e k8s_get_resources"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			var got []string
			for _, p := range tc.parts(t, tc.fixture) {
				if tp, ok := dialect.DecodeToolPart(p); ok {
					got = append(got, string(tp.Kind)+" "+tp.ID+" "+tp.Name)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tool parts = %q, want %q", got, tc.want)
			}
		})
	}
}
