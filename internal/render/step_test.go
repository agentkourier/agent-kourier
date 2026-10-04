package render

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// A step is a tool call's name and arguments as the tool line shows them: the same redaction, one line, the same caps.
// It is built by the same function, so that no second path puts arguments in front of a reader.
func TestStepIsTheToolLineAsAStepInTheStateGiven(t *testing.T) {
	call := agent.ToolCallEvent{CallID: "c1", Name: "kubectl_get", Args: map[string]any{"ns": "prod", "kind": "pods"}}
	line := Tool(call.Name, call.Args)
	for _, state := range []chat.StepState{chat.StepRunning, chat.StepDone, chat.StepFailed, chat.StepEnded} {
		got := Step("s1", call, state)
		want := chat.Step{ID: "s1", Title: line.Name, Detail: line.Summary, State: state}
		if got != want {
			t.Errorf("Step in %v = %+v, want %+v", state, got, want)
		}
	}
	if got := Step("s1", call, chat.StepRunning); got.Title != "kubectl_get" || got.Detail != `kind="pods", ns="prod"` {
		t.Errorf("Step = %+v, want the name as its title and the sorted arguments as its detail", got)
	}
}

// The arguments are redacted by the name they sit under and by the shape of what they hold, before they are shown or
// cut (spec 8.4), and the step never holds the raw arguments.
func TestStepNeverHoldsASecretOfTheArguments(t *testing.T) {
	step := Step("s1", agent.ToolCallEvent{Name: "run", Args: map[string]any{
		"password":      "hunter2hunter2",
		"Authorization": "Bearer abcdefghijklmnop",
		"cfg":           map[string]any{"DB_Password": "hunter2hunter2", "name": "db1"},
		"url":           "https://admin:s3cretpass@db.example/x",
		"cmd":           "curl -H 'Authorization: Bearer xoxb-123-456-abcdefghijkl' https://db.example",
	}}, chat.StepRunning)
	shown := step.Title + "\n" + step.Detail
	for _, secret := range []string{"hunter2hunter2", "s3cretpass", "abcdefghijklmnop", "xoxb-123-456", "admin:"} {
		if strings.Contains(shown, secret) {
			t.Errorf("a step shows %q: %q", secret, shown)
		}
	}
	if !strings.Contains(step.Detail, "db1") {
		t.Errorf("detail %q lost a value that is no secret", step.Detail)
	}
}

// A hostile tool name and arguments are one line each, with no hidden character, and capped, whatever the agent sent.
func TestStepTextIsOneLineCleanAndCapped(t *testing.T) {
	step := Step("s1", agent.ToolCallEvent{
		Name: "<!channel>\n\u202erun" + strings.Repeat("n", 500),
		Args: map[string]any{"q": "[x](https://evil.example)\r\n" + strings.Repeat("v", 5000), "<@U1>": "\u200bx"},
	}, chat.StepRunning)
	for name, s := range map[string]string{"title": step.Title, "detail": step.Detail} {
		if strings.ContainsAny(s, "\n\r\t\u202e\u200b") {
			t.Errorf("the %s %q is not one clean line", name, s)
		}
	}
	if chat.Units(step.Title) > maxToolName || chat.Units(step.Detail) > maxToolArgs {
		t.Errorf("title is %d units and detail %d, want at most %d and %d", chat.Units(step.Title), chat.Units(step.Detail), maxToolName, maxToolArgs)
	}
	if !strings.HasPrefix(step.Title, "<!channel> run") {
		t.Errorf("title = %q, want the name as it was, cleaned and not escaped (the adapter makes it inert)", step.Title)
	}
}

func TestFailedReadsAnErrorOutOfAToolResponse(t *testing.T) {
	for name, tc := range map[string]struct {
		response any
		want     bool
	}{
		"an error message":         {map[string]any{"error": "tool \"add_numbers\" call is rejected"}, true},
		"an error that is a map":   {map[string]any{"error": map[string]any{"code": 7}}, true},
		"an empty error":           {map[string]any{"error": ""}, false},
		"a null error":             {map[string]any{"error": nil}, false},
		"error true":               {map[string]any{"error": true}, true},
		"error false":              {map[string]any{"error": false}, false},
		"an empty map as error":    {map[string]any{"error": map[string]any{}}, false},
		"an empty list as error":   {map[string]any{"error": []any{}}, false},
		"a list of errors":         {map[string]any{"error": []any{"boom"}}, true},
		"a zero as error":          {map[string]any{"error": 0}, false},
		"a zero float as error":    {map[string]any{"error": float64(0)}, false},
		"an output":                {map[string]any{"output": "8"}, false},
		"a result that says error": {map[string]any{"result": "error: none, all is well"}, false},
		"MCP isError":              {map[string]any{"isError": true, "content": "x"}, true},
		"MCP isError false":        {map[string]any{"isError": false}, false},
		"is_error":                 {map[string]any{"is_error": true}, true},
		"a text response":          {"error", false},
		"nothing":                  {nil, false},
		"an empty map":             {map[string]any{}, false},
	} {
		if got := Failed(tc.response); got != tc.want {
			t.Errorf("%s: Failed(%v) = %t, want %t", name, tc.response, got, tc.want)
		}
	}
}
