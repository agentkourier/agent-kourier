package interact_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
)

// threeTools is an approval whose tools are listed in an order that is not the order of their IDs, so that a decision
// that was made by position in a sorted list, or by ID in the wrong order, shows.
var threeTools = dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", RequestID: "req-9", Tools: []dialect.Tool{
	{ID: "t-c", Name: "first"}, {ID: "t-a", Name: "second"}, {ID: "t-b", Name: "third"}}}

func verdict(tool int, v string) chat.Answer { return chat.Answer{Question: tool, Values: []string{v}} }

const (
	yes = chat.ToolApproveValue
	no  = chat.ToolDenyValue
)

// deny is the decision of a tool that was denied: the one fixed reason, and no word of a person's.
func deny(id string) dialect.Decision {
	return dialect.Decision{ToolID: id, Reason: "denied by an approver in chat"}
}

func approve(id string) dialect.Decision { return dialect.Decision{ToolID: id, Approved: true} }

// TestExtractDecisionsComplete: every way to give a verdict for every tool, each to one decision a tool, in the order of the
// request and by tool ID. A denial carries the one fixed reason and an approval none.
func TestExtractDecisionsComplete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		action  chat.Action
		answers []chat.Answer
		want    []dialect.Decision
	}{
		{"approve all", chat.ActionApproveAll, nil, []dialect.Decision{approve("t-c"), approve("t-a"), approve("t-b")}},
		{"deny all", chat.ActionDenyAll, nil, []dialect.Decision{deny("t-c"), deny("t-a"), deny("t-b")}},
		// The controls are ignored by the two buttons that decide everything: whatever they held, even a stray answer.
		{"approve all ignores the controls", chat.ActionApproveAll, []chat.Answer{verdict(0, no), verdict(7, "junk")}, []dialect.Decision{approve("t-c"), approve("t-a"), approve("t-b")}},
		{"deny all ignores the controls", chat.ActionDenyAll, []chat.Answer{verdict(0, yes), verdict(0, yes)}, []dialect.Decision{deny("t-c"), deny("t-a"), deny("t-b")}},
		{"submit, mixed", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(1, no), verdict(2, yes)}, []dialect.Decision{approve("t-c"), deny("t-a"), approve("t-b")}},
		{"submit, all approved", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(1, yes), verdict(2, yes)}, []dialect.Decision{approve("t-c"), approve("t-a"), approve("t-b")}},
		{"submit, all denied", chat.ActionSubmit, []chat.Answer{verdict(0, no), verdict(1, no), verdict(2, no)}, []dialect.Decision{deny("t-c"), deny("t-a"), deny("t-b")}},
		// The order of the answers is the adapter's, and the decisions are in the request's order whatever it is.
		{"submit, answers out of order", chat.ActionSubmit, []chat.Answer{verdict(2, no), verdict(0, yes), verdict(1, yes)}, []dialect.Decision{approve("t-c"), approve("t-a"), deny("t-b")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problems, err := interact.ExtractDecisions(threeTools, tc.action, tc.answers)
			if err != nil || len(problems) != 0 {
				t.Fatalf("ExtractDecisions = %+v, %v; want a complete decision", problems, err)
			}
			want := dialect.Answer{RequestID: "req-9", Decisions: tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("answer = %+v; want %+v", got, want)
			}
		})
	}
}

// TestExtractDecisionsOfOneToolWithNoVerdictIsIncomplete: a prompt of one tool has an Approve and a Deny button and no
// radio and no Submit, so a Submit of it holds no verdict. It is never a decision, however it got there: a stale or forged
// press must not approve a tool call.
func TestExtractDecisionsOfOneToolWithNoVerdictIsIncomplete(t *testing.T) {
	one := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", RequestID: "req-1", Tools: []dialect.Tool{{ID: "only", Name: "n"}}}
	for _, tc := range []struct {
		name string
		in   dialect.Interaction
	}{{"one tool", one}, {"several tools", threeTools}} {
		t.Run(tc.name, func(t *testing.T) {
			got, problems, err := interact.ExtractDecisions(tc.in, chat.ActionSubmit, nil)
			if err != nil || len(problems) != len(tc.in.Tools) || len(got.Decisions) != 0 {
				t.Fatalf("ExtractDecisions = %+v, %+v, %v; want every tool a problem and no decision", got, problems, err)
			}
			for i, p := range problems {
				if p != (interact.Problem{Question: i, Kind: interact.Unanswered}) {
					t.Errorf("problem %d = %+v; want the tool unanswered", i, p)
				}
			}
		})
	}
	// A Submit that holds a valid verdict for the one tool does decide: the neutral model has no layout, and another adapter may show
	// a radio for one tool. The Slack adapter cannot produce it.
	for v, want := range map[string]dialect.Decision{yes: approve("only"), no: deny("only")} {
		if got, problems, err := interact.ExtractDecisions(one, chat.ActionSubmit, []chat.Answer{verdict(0, v)}); err != nil || len(problems) != 0 || !reflect.DeepEqual(got.Decisions, []dialect.Decision{want}) {
			t.Errorf("a Submit of verdict %s on one tool = %+v, %+v, %v; want %+v", v, got, problems, err, want)
		}
	}
	if got, problems, err := interact.ExtractDecisions(one, chat.ActionApproveAll, nil); err != nil || len(problems) != 0 || !reflect.DeepEqual(got.Decisions, []dialect.Decision{approve("only")}) {
		t.Errorf("Approve on one tool = %+v, %+v, %v; want it approved", got, problems, err)
	}
	if got, problems, err := interact.ExtractDecisions(one, chat.ActionDenyAll, nil); err != nil || len(problems) != 0 || !reflect.DeepEqual(got.Decisions, []dialect.Decision{deny("only")}) {
		t.Errorf("Deny on one tool = %+v, %+v, %v; want it denied", got, problems, err)
	}
}

// TestExtractDecisionsIsIncompleteForAnythingButOneVerdictForEveryTool: the problem is named by the tool's index, and kinds
// are Unanswered (nothing usable was given) or Invalid (something no honest press holds).
func TestExtractDecisionsIsIncompleteForAnythingButOneVerdictForEveryTool(t *testing.T) {
	un, inv := interact.Unanswered, interact.Invalid
	for _, tc := range []struct {
		name    string
		action  chat.Action
		answers []chat.Answer
		want    []interact.Problem
	}{
		{"one tool left", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(2, no)}, []interact.Problem{{Question: 1, Kind: un}}},
		{"two tools left", chat.ActionSubmit, []chat.Answer{verdict(1, yes)}, []interact.Problem{{Question: 0, Kind: un}, {Question: 2, Kind: un}}},
		{"a tool answered twice, the same way", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(0, yes), verdict(1, no), verdict(2, no)}, []interact.Problem{{Question: 0, Kind: inv}}},
		{"a tool answered twice, both ways", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(0, no), verdict(1, no), verdict(2, no)}, []interact.Problem{{Question: 0, Kind: inv}}},
		{"a value that is no verdict", chat.ActionSubmit, []chat.Answer{verdict(0, "2"), verdict(1, no), verdict(2, no)}, []interact.Problem{{Question: 0, Kind: inv}}},
		{"approve, written another way", chat.ActionSubmit, []chat.Answer{verdict(0, "00"), verdict(1, "+1"), verdict(2, "approve")}, []interact.Problem{{Question: 0, Kind: inv}, {Question: 1, Kind: inv}, {Question: 2, Kind: inv}}},
		{"an empty value", chat.ActionSubmit, []chat.Answer{verdict(0, ""), verdict(1, yes), verdict(2, yes)}, []interact.Problem{{Question: 0, Kind: inv}}},
		{"no values", chat.ActionSubmit, []chat.Answer{{Question: 0}, verdict(1, yes), verdict(2, yes)}, []interact.Problem{{Question: 0, Kind: inv}}},
		{"two values", chat.ActionSubmit, []chat.Answer{{Question: 0, Values: []string{yes, no}}, verdict(1, yes), verdict(2, yes)}, []interact.Problem{{Question: 0, Kind: inv}}},
		// A tool the prompt does not have is no honest press: nothing in it is read, every tool is refused, even one it decided right.
		{"an index past the last tool", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(1, yes), verdict(2, yes), verdict(3, yes)}, []interact.Problem{{Question: 0, Kind: inv}, {Question: 1, Kind: inv}, {Question: 2, Kind: inv}}},
		{"a negative index", chat.ActionSubmit, []chat.Answer{verdict(0, yes), verdict(1, yes), verdict(2, yes), verdict(-1, yes)}, []interact.Problem{{Question: 0, Kind: inv}, {Question: 1, Kind: inv}, {Question: 2, Kind: inv}}},
		{"an action this version does not know", chat.Action(9), nil, []interact.Problem{{Question: 0, Kind: inv}, {Question: 1, Kind: inv}, {Question: 2, Kind: inv}}},
		{"a negative action", chat.Action(-1), []chat.Answer{verdict(0, yes), verdict(1, yes), verdict(2, yes)}, []interact.Problem{{Question: 0, Kind: inv}, {Question: 1, Kind: inv}, {Question: 2, Kind: inv}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problems, err := interact.ExtractDecisions(threeTools, tc.action, tc.answers)
			if err != nil || !reflect.DeepEqual(problems, tc.want) {
				t.Fatalf("ExtractDecisions = %+v, %v; want the problems %+v", problems, err, tc.want)
			}
			if len(got.Decisions) != 0 {
				t.Errorf("decisions %+v from an incomplete press; want none", got.Decisions)
			}
		})
	}
}

// TestExtractDecisionsReadsNothingForWhatIsNotAnApproval: a question, or an approval of no tools, has nothing to decide.
func TestExtractDecisionsReadsNothingForWhatIsNotAnApproval(t *testing.T) {
	for name, in := range map[string]dialect.Interaction{
		"a question":          question,
		"an approval of none": {Kind: dialect.ToolApproval, RequestID: "r"},
		"no kind":             {Tools: threeTools.Tools},
	} {
		if got, problems, err := interact.ExtractDecisions(in, chat.ActionApproveAll, nil); !errors.Is(err, interact.ErrNotToolApproval) || len(problems) != 0 || len(got.Decisions) != 0 {
			t.Errorf("%s: ExtractDecisions = %+v, %+v, %v; want ErrNotToolApproval and nothing read", name, got, problems, err)
		}
	}
	if _, _, err := interact.ExtractAnswer(threeTools, nil); !errors.Is(err, interact.ErrNotAskUser) {
		t.Errorf("ExtractAnswer of an approval = %v; want ErrNotAskUser still", err)
	}
}
