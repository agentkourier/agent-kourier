package kagentv1

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// kagent marks each tool call and tool result artifact with a part-type in the metadata of
// its data part (WP-02, measured at alpha5; test/fixtures/kagent/approval).
const (
	partTypeKey          = "kagent.dev/a2a/part-type"
	partFunctionCall     = "function_call"
	partFunctionResponse = "function_response"
)

// ToolPart recognizes kagent's function_call and function_response data parts. Any other
// part, including a data part of another type or with a shape it does not expect, is not a
// tool part.
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
	switch part.Metadata[partTypeKey] {
	case partFunctionCall:
		args, _ := data["args"].(map[string]any)
		return dialect.ToolPart{Kind: dialect.ToolCall, ID: id, Name: name, Args: args}, true
	case partFunctionResponse:
		return dialect.ToolPart{Kind: dialect.ToolResult, ID: id, Name: name, Result: data["response"]}, true
	}
	return dialect.ToolPart{}, false
}
