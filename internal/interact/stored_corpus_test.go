package interact

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// A stored request is normalized and redacted whole: a secret that an invisible character splits, in the hint, a
// question, a tool's name or arguments or a sub-agent's name, is not kept (spec 9 and 10 promise redaction before
// storing). A question's choices are the one thing stored as given, so that an answer picks one by its index.
func TestStorableTakesTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		got := storable(dialect.Interaction{
			Kind: dialect.AskUser, TaskID: "task-1", RequestID: "req-1",
			Hint:      c.Text,
			Questions: []dialect.Question{{Text: c.Text}},
			Tools:     []dialect.Tool{{ID: "t1", Name: c.Text, Args: map[string]any{"note": c.Text, "nested": []any{c.Text}}}},
			Nested:    &dialect.Nested{Subagent: c.Text},
		})
		c.CheckKept(t, got.Hint)
		c.CheckKept(t, got.Questions[0].Text)
		c.CheckKept(t, got.Tools[0].Name)
		c.CheckKept(t, got.Tools[0].Args["note"].(string))
		c.CheckKept(t, got.Tools[0].Args["nested"].([]any)[0].(string))
		c.CheckKept(t, got.Nested.Subagent)
	}
}
