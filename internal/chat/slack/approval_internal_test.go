package slack

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

func TestSharesAreWhatEachNeedsOrAnEqualPartOfWhatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		needs  []int
		budget int
		want   []int
	}{
		{[]int{10, 20}, 100, []int{10, 20}},
		{[]int{100, 100}, 100, []int{50, 50}},
		{[]int{10, 100, 100}, 100, []int{10, 45, 45}},
		{[]int{100, 10, 100}, 100, []int{45, 10, 45}},
		{[]int{0, 0, 90}, 60, []int{0, 0, 60}},
		{nil, 100, []int{}},
	} {
		if got := shares(tc.needs, tc.budget); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("shares(%v, %d) = %v, want %v", tc.needs, tc.budget, got, tc.want)
		}
	}
}

// The values of the Approve and Deny options are the indices the neutral model reads as a verdict.
func TestTheOptionsOfAToolAreValuedAsTheNeutralModelReadsThem(t *testing.T) {
	opts, ok := options([]string{approveWord, denyWord})
	if !ok || len(opts) != 2 || opts[0]["value"] != chat.ToolApproveValue || opts[1]["value"] != chat.ToolDenyValue {
		t.Fatalf("options = %v, %t", opts, ok)
	}
	for value, want := range map[string]chat.ToolVerdict{opts[0]["value"].(string): chat.ToolApproved, opts[1]["value"].(string): chat.ToolDenied} {
		if got, ok := (chat.Answer{Values: []string{value}}).ToolVerdict(); !ok || got != want {
			t.Errorf("value %q reads as %q, %t; want %q", value, got, ok, want)
		}
	}
}

// Which button was pressed is its action ID. Approve and Approve all, Deny and Deny all, are one action each and
// ignore the controls. The controls are read for the Submit button and no other. A button the adapter does not render
// reads no controls and makes no action: in a block the adapter renders for a prompt (submit, decide) it is not
// the prompt's, and its press names no prompt (an empty PromptID, which the pipeline and the feedback handler both
// drop as someone else's button); in another block it is a button of a message part (a feedback button), whose value
// is its own, and which carries no answers.
func TestPlatformInteractionDecodesTheButtonsOfAnApproval(t *testing.T) {
	at := chat.Ref{Platform: "slack", Conn: "ns/conn", Channel: "C1", Thread: "1.1", ID: "1.2"}
	radios := map[string]map[string]Input{"q0": {"r0": in("radio_buttons", "0")}, "q2": {"r2": in("radio_buttons", "1")}}
	press := func(action, block string, inputs map[string]map[string]Input) Interaction {
		return Interaction{User: "U1", Channel: "C1", ThreadTS: "1.1", MessageTS: "1.2", ActionID: action, BlockID: block, Value: "ix-1", Inputs: inputs}
	}
	verdicts := []chat.Answer{{Question: 0, Values: []string{"0"}}, {Question: 2, Values: []string{"1"}}}
	for _, tc := range []struct {
		name string
		in   Interaction
		want chat.Interaction
	}{
		{"approve (one tool) or approve all", press("approve_all", "decide", nil), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionApproveAll}},
		{"deny (one tool) or deny all", press("deny_all", "submit", nil), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionDenyAll}},
		{"approve all ignores the radios", press("approve_all", "submit", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionApproveAll}},
		{"deny all ignores the radios", press("deny_all", "submit", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionDenyAll}},
		{"submit carries a verdict for each radio that is set", press("submit", "submit", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Answers: verdicts}},
		{"submit with no radio set carries no answer", press("submit", "submit", nil), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"a button of the prompt's blocks that is not the adapter's names no prompt", press("approve_every_tool", "submit", radios), chat.Interaction{User: "U1", At: at}},
		{"the same in the block of one tool", press("approve_every_tool", "decide", radios), chat.Interaction{User: "U1", At: at}},
		{"no action ID in the prompt's block names no prompt", press("", "submit", radios), chat.Interaction{User: "U1", At: at}},
		{"a look-alike action ID is not approve all", press("Approve_All", "submit", radios), chat.Interaction{User: "U1", At: at}},
		{"a button of a message part keeps its value and reads no controls", press("feedback_up", "buttons3", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		// The decision buttons are the prompt's own: each only in the block that holds it. In the block of a message part
		// they are any button, which keeps its value for its own handler and carries no action and no answers.
		{"approve all in a message part's block carries no action", press("approve_all", "buttons0", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"deny all in a message part's block carries no action", press("deny_all", "buttons0", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"submit in a message part's block carries no answers", press("submit", "buttons0", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"approve all in a block of no ID carries no action", press("approve_all", "", nil), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"submit is not a button of the block of one tool", press("submit", "decide", radios), chat.Interaction{User: "U1", At: at}},
		{"approve all is a button of the block of one tool", press("approve_all", "decide", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionApproveAll}},
		{"deny all is a button of the block of one tool", press("deny_all", "decide", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionDenyAll}},
		{"approve all is a button of the block of several tools", press("approve_all", "submit", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionApproveAll}},
		{"deny all is a button of the block of several tools", press("deny_all", "submit", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1", Action: chat.ActionDenyAll}},
		// The block's ID is matched whole and as written: a look-alike block is a message part's, not the prompt's.
		{"approve all in a block named in another case carries no action", press("approve_all", "Submit", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"approve all in a block whose ID only starts as the prompt's carries no action", press("approve_all", "decide2", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
		{"submit in a block whose ID only starts as the prompt's carries no answers", press("submit", "submit0", radios), chat.Interaction{User: "U1", At: at, PromptID: "ix-1"}},
	} {
		if got := NewPlatform(nil, "ns/conn").interaction(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: interaction = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
