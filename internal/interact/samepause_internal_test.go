package interact

import (
	"encoding/json"
	"strings"
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

// TestSamePauseSecondReadingIsForAQuestionOnly: a stored request that equals the fresh pause only once it is redacted again
// (the case of a row written before secrets were taken out) is the same question, and is not the same tool approval: no
// approval row was written so, and for stored text over 4 KiB the second reading lets a pause that was constructed match a
// row whose text was never checked.
func TestSamePauseSecondReadingIsForAQuestionOnly(t *testing.T) {
	question := dialect.Interaction{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "r",
		Questions: []dialect.Question{{Text: "Is password=planted-secret-in-the-question right?", Choices: []string{"yes", "no"}}}}
	approval := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", RequestID: "r",
		Tools: []dialect.Tool{{ID: "t1", Name: "run", Args: map[string]any{"password": "planted-password"}}}}
	long := approval // text over 4 KiB, which the redaction cuts: the stored form of a constructed pause
	long.Tools = []dialect.Tool{{ID: "t1", Name: "run", Args: map[string]any{"command": strings.Repeat("a", 5000)}}}
	for name, tc := range map[string]struct {
		stored  dialect.Interaction
		already bool // the stored request is the fresh pause's stored form, byte for byte
	}{
		"a question that equals it once redacted again":        {question, false},
		"an approval that equals it once redacted again":       {approval, false},
		"an approval whose text the redaction would cut":       {long, false},
		"an approval that is its own stored form":              {storable(approval), true},
		"a question that is its own stored form":               {storable(question), true},
		"an approval of long text that is its own stored form": {storable(long), true},
	} {
		stored, _ := json.Marshal(tc.stored)
		fresh, _ := json.Marshal(storable(tc.stored))
		if (string(stored) == string(fresh)) != tc.already {
			t.Fatalf("%s: the case does not test what it says: the stored request is its fresh pause's stored form: %v", name, !tc.already)
		}
		// The second reading is the only way a request that is not already the stored form can match, and it is a question's.
		want := tc.already || tc.stored.Kind != dialect.ToolApproval
		if got := samePause(stored, fresh); got != want {
			t.Errorf("%s: samePause = %v; want %v", name, got, want)
		}
	}
}
