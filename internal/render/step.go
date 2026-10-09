package render

import (
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Step returns the progress step of a tool call in the state given: the tool's name as its title and the redacted summary
// of its arguments as its detail. They are the name and summary of the tool line (Tool), the same redacted summary that
// the status line of the update path shows, put on one line and capped as it is: the raw arguments never reach a step. id is Agent Kourier's own name for the step in its stream, never the agent's call ID.
func Step(id string, call agent.ToolCallEvent, state chat.StepState) chat.Step {
	line := Tool(call.Name, call.Args)
	return chat.Step{ID: id, Title: line.Name, Detail: line.Summary, State: state}
}

// Failed says whether the response of a tool call reports that the call failed. A response is whatever the tool
// returned, so it is read for the two shapes that are known: an "error" entry that holds something, as kagent reports a
// rejected or unconfirmed call (test/fixtures/kagent/approval), and an "isError" or "is_error" entry that is true, as an
// MCP tool does. An error entry holds something if it is true, a string that is not empty, or a map or a list that is
// not: false, null, "", {}, [] and a number, such as the 0 of a status, say that nothing went wrong or say nothing. Anything
// else, including text that says "error", is a success: a tool's own words are the agent's to report.
func Failed(response any) bool {
	m, ok := response.(map[string]any)
	if !ok {
		return false
	}
	switch e := m["error"].(type) {
	case bool:
		if e {
			return true
		}
	case string:
		if e != "" {
			return true
		}
	case map[string]any:
		if len(e) > 0 {
			return true
		}
	case []any:
		if len(e) > 0 {
			return true
		}
	}
	for _, key := range []string{"isError", "is_error"} {
		if failed, _ := m[key].(bool); failed {
			return true
		}
	}
	return false
}
