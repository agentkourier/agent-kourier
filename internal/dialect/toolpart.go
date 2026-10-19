package dialect

import "github.com/a2aproject/a2a-go/v2/a2a"

// A2A defines no standard for a tool call, so the tool parts below are conventions of the agent
// frameworks that write them, each marking the data part it sends in its metadata (spec D45):
// Google ADK's A2A converter (adk_type), kagent 0.9.x, which reuses that converter with its own prefix
// (kagent_type, measured at 0.9.4; test/fixtures/kagent/v09-tools), and kagent 1.0
// (kagent.dev/a2a/part-type, measured at alpha5; test/fixtures/kagent/approval).
var toolPartMarkerKeys = [...]string{"adk_type", "kagent_type", "kagent.dev/a2a/part-type"}

const (
	markerFunctionCall     = "function_call"
	markerFunctionResponse = "function_response"
)

// DecodeToolPart classifies a data part as a tool call or a tool result, the same way on every
// dialect. The part's metadata must hold exactly one of the marker keys (adk_type, kagent_type,
// kagent.dev/a2a/part-type) with the string value function_call or function_response: a key that
// is absent, null or holds another value does not count, and two keys that hold a marker are
// ambiguous and decode nothing. The data must be an object with a non-empty string name; id, args
// and response are read as ADK writes them. Any other part is not a tool part, and the caller
// treats it as plain content. That includes ADK's other types (tool_call, tool_response,
// executable_code, code_execution_result), until a case needs them.
func DecodeToolPart(part *a2a.Part) (ToolPart, bool) {
	if part == nil {
		return ToolPart{}, false
	}
	data, ok := part.Data().(map[string]any)
	if !ok {
		return ToolPart{}, false
	}
	var kind ToolPartKind
	markers := 0
	for _, key := range toolPartMarkerKeys {
		switch part.Metadata[key] {
		case markerFunctionCall:
			kind, markers = ToolCall, markers+1
		case markerFunctionResponse:
			kind, markers = ToolResult, markers+1
		}
	}
	name, _ := data["name"].(string)
	if markers != 1 || name == "" {
		return ToolPart{}, false
	}
	id, _ := data["id"].(string)
	if kind == ToolCall {
		args, _ := data["args"].(map[string]any)
		return ToolPart{Kind: ToolCall, ID: id, Name: name, Args: args}, true
	}
	return ToolPart{Kind: ToolResult, ID: id, Name: name, Result: data["response"]}, true
}
