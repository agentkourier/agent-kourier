package agent

import (
	"reflect"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
)

// finalOf is the FinalEvent of evs, which must have one.
func finalOf(t *testing.T, evs []Event) FinalEvent {
	t.Helper()
	for _, e := range evs {
		if f, ok := e.(FinalEvent); ok {
			return f
		}
	}
	t.Fatalf("no final event in %#v", evs)
	return FinalEvent{}
}

// The final event of a pause for a tool approval names the calls it waits on, as the dialect reads the pause, so that a
// renderer can show them as waiting. Any other end names none: a question, a task that completed, a pause the dialect
// cannot read, one a sub-agent raised (its calls are not this stream's), and a pause read with no dialect at all.
func TestTheFinalOfAnApprovalNamesTheCallsItWaitsOn(t *testing.T) {
	kagent := kagentv1.New(time.Second).ParseInteraction
	approval := fixtureEvents(t, "approval/01-pause-approve.response.sse")
	for _, tc := range []struct {
		name  string
		read  func(a2a.Task) (*dialect.Interaction, error)
		evs   []a2a.Event
		wants []string
	}{
		{"a tool approval", kagent, approval, []string{"call_1"}},
		{"a tool approval with no dialect", nil, approval, nil},
		{"a question", kagent, fixtureEvents(t, "askuser/03-pause-good-answer.response.sse"), nil},
		{"a completed task", kagent, []a2a.Event{taskOf(a2a.TaskStateCompleted, nil)}, nil},
		{"a pause the dialect cannot read", func(a2a.Task) (*dialect.Interaction, error) { return nil, dialect.ErrUnusablePause }, approval, nil},
		{"an approval a sub-agent raised", func(a2a.Task) (*dialect.Interaction, error) {
			return &dialect.Interaction{Kind: dialect.ToolApproval, Tools: []dialect.Tool{{ID: "x", CallID: "call_1"}}, Nested: &dialect.Nested{Subagent: "child"}}, nil
		}, approval, nil},
		{"an approval of a call with no ID", func(a2a.Task) (*dialect.Interaction, error) {
			return &dialect.Interaction{Kind: dialect.ToolApproval, Tools: []dialect.Tool{{ID: "x"}, {ID: "y", CallID: "c2"}}}, nil
		}, approval, []string{"c2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Event
			asm := newAssembler(func(e Event) { got = append(got, e) })
			asm.interaction = tc.read
			for _, ev := range tc.evs {
				asm.apply(ev)
			}
			if f := finalOf(t, got); !reflect.DeepEqual(f.AwaitingApproval, tc.wants) {
				t.Fatalf("AwaitingApproval = %q, want %q", f.AwaitingApproval, tc.wants)
			}
		})
	}
}

// A Pool's Agents read pauses with its dialect.
func TestAPoolsAgentsReadPausesWithItsDialect(t *testing.T) {
	p := NewPool(kagentv1.New(0), Config{})
	t.Cleanup(p.Close)
	read := p.Agent(Target{}).cfg.Interaction
	if read == nil {
		t.Fatal("Config.Interaction is nil; want the dialect's ParseInteraction")
	}
	evs := fixtureEvents(t, "approval/01-pause-approve.response.sse")
	if in, err := read(*evs[len(evs)-1].(*a2a.Task)); err != nil || in == nil || in.Kind != dialect.ToolApproval {
		t.Fatalf("Interaction = %+v, %v; want the tool approval kagent's dialect reads", in, err)
	}
}
