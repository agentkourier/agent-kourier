package agent

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

// progress lists the ProgressEvents as their text, "-" for one with none.
func progress(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if p, ok := e.(ProgressEvent); ok {
			if p.Text == "" {
				out = append(out, "-")
			} else {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

func textOf(s string) *a2a.Part { return a2a.NewTextPart(s) }

// A live working update that holds no tool part is the agent's activity, whether or not anything in it is understood, and the
// text of the agent's own message is its progress line (spec D45 (1)). The update that makes the task working is the change of
// state, and progress only if it says something.
func TestWorkingUpdatesAreProgress(t *testing.T) {
	working := func(m *a2a.Message) a2a.Event { return statusOf(a2a.TaskStateWorking, m) }
	agentSays := func(id string, parts ...*a2a.Part) *a2a.Message { return messageOf(a2a.MessageRoleAgent, id, parts...) }
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"the first update, with no message, is the change of state", []a2a.Event{working(nil)}, nil},
		{"each update after it is one unit", []a2a.Event{working(nil), working(nil), working(nil)}, []string{"-", "-"}},
		{"the first update with text says something", []a2a.Event{working(agentSays("m1", textOf("Looking at the pods")))}, []string{"Looking at the pods"}},
		{"the agent's text", []a2a.Event{working(nil), working(agentSays("m1", textOf("Reading logs")))}, []string{"Reading logs"}},
		{"text parts joined by line breaks", []a2a.Event{working(nil), working(agentSays("m1", textOf("one"), textOf("two")))}, []string{"one\ntwo"}},
		{"a data part that is no tool part is opaque", []a2a.Event{working(nil), working(agentSays("m1", a2a.NewDataPart(map[string]any{"x": 1.0})))}, []string{"-"}},
		{"text beside an opaque data part", []a2a.Event{working(nil), working(agentSays("m1", a2a.NewDataPart(map[string]any{"x": 1.0}), textOf("ok")))}, []string{"ok"}},
		{"the user's message echoed back has no text of the agent's", []a2a.Event{working(nil), working(messageOf(a2a.MessageRoleUser, "u1", textOf("yes")))}, []string{"-"}},
		{"a message with no role has none either", []a2a.Event{working(nil), working(messageOf(a2a.MessageRoleUnspecified, "m1", textOf("hi")))}, []string{"-"}},
		{"an update with a tool call is its tool event", []a2a.Event{working(nil), working(agentSays("m1", callPart(v09, "c1"), textOf("calling")))}, nil},
		{"an update with a tool result is its tool event", []a2a.Event{working(nil), working(agentSays("m1", resultPart(adk, "c1", okResp)))}, nil},
		{"a repeated tool part is still a tool part", []a2a.Event{working(nil), working(agentSays("m1", callPart(adk, "c1"))), working(agentSays("m1", callPart(adk, "c1")))}, nil},
		{"a user's echo with a tool part is no progress", []a2a.Event{working(nil), working(messageOf(a2a.MessageRoleUser, "u1", callPart(adk, "c1")))}, nil},
		{"a pause is no progress", []a2a.Event{working(nil), statusOf(a2a.TaskStateInputRequired, agentSays("m1", textOf("Which one?")))}, nil},
		{"the end is no progress", []a2a.Event{working(nil), statusOf(a2a.TaskStateCompleted, agentSays("m1", textOf("Done")))}, nil},
		{"back to working after a pause is the change of state", []a2a.Event{working(nil), statusOf(a2a.TaskStateInputRequired, nil), working(nil)}, nil},
		{"a snapshot is no progress", []a2a.Event{working(nil), taskOf(a2a.TaskStateWorking, agentSays("m1", textOf("Reading logs")))}, nil},
		{"an update after a snapshot that made the task working", []a2a.Event{taskOf(a2a.TaskStateWorking, nil), working(agentSays("m1", textOf("Reading logs")))}, []string{"Reading logs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(progress(got), tc.want) {
				t.Errorf("progress = %q, want %q", progress(got), tc.want)
			}
		})
	}
}

// A ProgressEvent comes where its update did, among the other events, so that a renderer reads it as the latest news.
func TestProgressComesInOrder(t *testing.T) {
	_, got := run([]a2a.Event{
		statusOf(a2a.TaskStateWorking, nil),
		statusOf(a2a.TaskStateWorking, messageOf(a2a.MessageRoleAgent, "m1", textOf("Reading logs"))),
		toolArtifact("a1", callPart(kagv1, "c1")),
		statusOf(a2a.TaskStateWorking, nil),
	})
	var kinds []string
	for _, e := range got {
		switch e.(type) {
		case StateEvent:
			kinds = append(kinds, "state")
		case ProgressEvent:
			kinds = append(kinds, "progress")
		case ToolCallEvent:
			kinds = append(kinds, "call")
		}
	}
	if want := []string{"state", "progress", "call", "progress"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("events = %q, want %q", kinds, want)
	}
}

// kagent 0.9.x over A2A 0.3 (test/fixtures/kagent/v09-tools): every update after the first holds a tool part but the last,
// which repeats the answer that its artifact holds. That one is progress, and its text the answer (accepted, spec D45 (1)).
func TestKagent09TurnHasOneProgressUpdate(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "tasks/get" {
			reply(t, w, "v09-tools/02-gettask-after-tool-turn.response.json")
			return
		}
		sse(t, w, "v09-tools/01-tool-turn.response.sse", -1)
	})
	fa.v03 = true
	h, got := collector()
	if err := fa.agentVia(t, generic.New(0)).Send(t.Context(), message("How many namespaces?"), h); err != nil {
		t.Fatal(err)
	}
	if p := progress(*got); !reflect.DeepEqual(p, []string{"The cluster has 46 namespaces."}) {
		t.Errorf("progress = %q, want the answer's text once", p)
	}
}

// kagent 1.0's approving turn (test/fixtures/kagent/approval/02-approve) echoes the user's answer in a working update: activity,
// with no text, as is the empty update after it.
func TestKagentApprovalEchoIsProgressWithNoText(t *testing.T) {
	_, got := run(fixtureEvents(t, "approval/02-approve.response.sse"))
	if p := progress(got); !reflect.DeepEqual(p, []string{"-", "-"}) {
		t.Errorf("progress = %q, want two with no text", p)
	}
}
