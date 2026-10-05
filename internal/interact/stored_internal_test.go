package interact

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// TestStorableTakesTheSecretsOutOfEveryFreeTextField and leaves alone what an answer is built from, and the pause it
// was given: it is posted, and answered, from the original.
func TestStorableTakesTheSecretsOutOfEveryFreeTextField(t *testing.T) {
	const secret = "planted-secret-value"
	in := dialect.Interaction{
		Kind: dialect.AskUser, TaskID: "task-1", ContextID: "ctx-1", RequestID: "req-1", TextReplies: true,
		Hint:      "use token=" + secret,
		Questions: []dialect.Question{{Text: "password: " + secret, Choices: []string{"keep password=" + secret, "no"}, Multiple: true}},
		Tools:     []dialect.Tool{{ID: "t1", CallID: "c1", Name: "run --password " + secret, Args: map[string]any{"api_key": secret, "n": 1, "list": []any{"x"}}}},
		Nested:    &dialect.Nested{Subagent: "agent token=" + secret, TaskID: "child-1", ContextID: "child-ctx"},
	}
	before := in.Tools[0].Args["api_key"]
	got := storable(in)

	for name, text := range map[string]string{"hint": got.Hint, "question": got.Questions[0].Text, "tool name": got.Tools[0].Name, "subagent": got.Nested.Subagent} {
		if strings.Contains(text, secret) {
			t.Errorf("the %s still holds the secret: %q", name, text)
		}
	}
	if got.Tools[0].Args["api_key"] != "[redacted]" || got.Tools[0].Args["n"] != 1 || !reflect.DeepEqual(got.Tools[0].Args["list"], []any{"x"}) {
		t.Errorf("tool arguments %v; want the secret one redacted and the rest as they were", got.Tools[0].Args)
	}

	// What an answer is built from, and the rest of the pause, is as it was: the choices exactly (the agent receives the stored one).
	want := in
	want.Hint, want.Questions, want.Tools, want.Nested = got.Hint, got.Questions, got.Tools, got.Nested
	if !reflect.DeepEqual(got, want) {
		t.Errorf("storable changed more than the free text: %+v", got)
	}
	if !reflect.DeepEqual(got.Questions[0].Choices, in.Questions[0].Choices) || got.Questions[0].Multiple != true || got.Nested.TaskID != "child-1" || got.Tools[0].ID != "t1" {
		t.Errorf("a choice, flag or ID changed: %+v", got)
	}

	// The original is not touched.
	if in.Hint != "use token="+secret || in.Questions[0].Text != "password: "+secret || in.Tools[0].Args["api_key"] != before || in.Nested.Subagent != "agent token="+secret {
		t.Errorf("storable changed its argument: %+v", in)
	}
	if empty := storable(dialect.Interaction{}); !reflect.DeepEqual(empty, dialect.Interaction{}) {
		t.Errorf("storable of nothing = %+v; want nothing", empty)
	}
}
