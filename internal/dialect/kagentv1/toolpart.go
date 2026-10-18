package kagentv1

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// kagent marks each tool call and tool result artifact with a part-type in the metadata of
// its data part (WP-02, measured at alpha5; test/fixtures/kagent/approval). kagent 0.9.x marks
// the same data parts with kagent_type instead, and sends them as status messages
// (measured at 0.9.4; test/fixtures/kagent/v09-tools).
const (
	partTypeKey          = "kagent.dev/a2a/part-type"
	v09PartTypeKey       = "kagent_type"
	partFunctionCall     = "function_call"
	partFunctionResponse = "function_response"
)

// ToolPart recognizes kagent's function_call and function_response data parts, as 1.0 or 0.9.x
// marks them. Any other part, including a data part of another type or with a shape it does not
// expect, is not a tool part.
func (*Dialect) ToolPart(part *a2a.Part) (dialect.ToolPart, bool) {
	if part == nil {
		return dialect.ToolPart{}, false
	}
	data, ok := part.Data().(map[string]any)
	if !ok {
		return dialect.ToolPart{}, false
	}
	id, _ := data["id"].(string)
	name, _ := data["name"].(string)
	partType, marked := part.Metadata[partTypeKey]
	if !marked {
		partType = part.Metadata[v09PartTypeKey]
	}
	switch partType {
	case partFunctionCall:
		args, _ := data["args"].(map[string]any)
		return dialect.ToolPart{Kind: dialect.ToolCall, ID: id, Name: name, Args: args}, true
	case partFunctionResponse:
		return dialect.ToolPart{Kind: dialect.ToolResult, ID: id, Name: name, Result: data["response"]}, true
	}
	return dialect.ToolPart{}, false
}
