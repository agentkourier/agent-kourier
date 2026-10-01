package interact

import (
	"encoding/json"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// TestSamePauseByWhatTheAnswerDependsOn: tools are compared by ID, name and arguments, in the order of
// their IDs (as InteractionID takes them), and not by their call IDs or the order the agent listed them.
func TestSamePauseByWhatTheAnswerDependsOn(t *testing.T) {
	tools := func(ts ...dialect.Tool) dialect.Interaction {
		return dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", RequestID: "r", Tools: ts}
	}
	rm := dialect.Tool{ID: "t1", CallID: "c1", Name: "rm", Args: map[string]any{"path": "/a"}}
	ls := dialect.Tool{ID: "t2", CallID: "c2", Name: "ls", Args: map[string]any{"path": "/b", "n": 3.0}}
	base := tools(rm, ls)
	other := func(f func(*dialect.Tool)) dialect.Tool {
		t := rm
		t.Args = map[string]any{"path": "/a"}
		f(&t)
		return t
	}
	for name, tc := range map[string]struct {
		fresh dialect.Interaction
		same  bool
	}{
		"the same":                     {base, true},
		"other call IDs":               {tools(other(func(t *dialect.Tool) { t.CallID = "other" }), ls), true},
		"the tools in another order":   {tools(ls, rm), true},
		"other arguments":              {tools(other(func(t *dialect.Tool) { t.Args["path"] = "/etc" }), ls), false},
		"an argument added":            {tools(other(func(t *dialect.Tool) { t.Args["force"] = true }), ls), false},
		"another name":                 {tools(other(func(t *dialect.Tool) { t.Name = "shred" }), ls), false},
		"another tool ID":              {tools(other(func(t *dialect.Tool) { t.ID = "t9" }), ls), false},
		"a tool missing":               {tools(rm), false},
		"the same tools, another kind": {dialect.Interaction{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "r", Tools: base.Tools}, false},
	} {
		stored, _ := json.Marshal(base)
		fresh, _ := json.Marshal(tc.fresh)
		if got := samePause(stored, fresh); got != tc.same {
			t.Errorf("%s: samePause = %v; want %v", name, got, tc.same)
		}
	}
}
