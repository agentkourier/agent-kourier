package dialect

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestStoredFormat pins the JSON of Interaction and Answer, which the interaction store keeps and a
// later version reads back: a renamed field would decode old rows as zero values, silently. Each
// golden string must decode to the value and encode back to the same string.
func TestStoredFormat(t *testing.T) {
	for name, tc := range map[string]struct {
		golden string
		into   func() any
		want   any
	}{
		"an interaction": {
			`{"kind":"ask_user","task_id":"task-1","context_id":"ctx-1","hint":"pick","request_id":"req-1",` +
				`"questions":[{"text":"Which?","choices":["a","b"],"multiple":true}],` +
				`"tools":[{"id":"t1","call_id":"c1","name":"add","args":{"a":3}}],` +
				`"nested":{"subagent":"specialist","task_id":"task-2","context_id":"ctx-2"},"extension":"https://example.test/ext"}`,
			func() any { return new(Interaction) },
			&Interaction{Kind: AskUser, TaskID: "task-1", ContextID: "ctx-1", Hint: "pick", RequestID: "req-1",
				Questions: []Question{{Text: "Which?", Choices: []string{"a", "b"}, Multiple: true}},
				Tools:     []Tool{{ID: "t1", CallID: "c1", Name: "add", Args: map[string]any{"a": 3.0}}},
				Nested:    &Nested{Subagent: "specialist", TaskID: "task-2", ContextID: "ctx-2"}, Extension: "https://example.test/ext"},
		},
		"an answer": {
			`{"request_id":"req-1","answers":[["a"]],"decisions":[{"tool_id":"t1","approved":false,"reason":"no"}]}`,
			func() any { return new(Answer) },
			&Answer{RequestID: "req-1", Answers: [][]string{{"a"}}, Decisions: []Decision{{ToolID: "t1", Reason: "no"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := tc.into()
			if err := json.Unmarshal([]byte(tc.golden), got); err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decoded %+v, %v; want %+v", got, err, tc.want)
			}
			if out, err := json.Marshal(tc.want); err != nil || string(out) != tc.golden {
				t.Errorf("encoded %s, %v; want %s", out, err, tc.golden)
			}
		})
	}
}
