package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
)

// The three markers a tool part may carry (dialect.DecodeToolPart): Google ADK, kagent 0.9.x, kagent 1.0.
const (
	adk   = "adk_type"
	v09   = "kagent_type"
	kagv1 = "kagent.dev/a2a/part-type"
)

func markedPart(key, kind string, data map[string]any) *a2a.Part {
	p := a2a.NewDataPart(data)
	p.Metadata = map[string]any{key: kind}
	return p
}

// callPart is a function_call data part for call id.
func callPart(key, id string) *a2a.Part {
	return markedPart(key, "function_call", map[string]any{"id": id, "name": "get", "args": map[string]any{"n": 1.0}})
}

// resultPart is a function_response data part for call id whose response is the object response.
func resultPart(key, id string, response map[string]any) *a2a.Part {
	return markedPart(key, "function_response", map[string]any{"id": id, "name": "get", "response": response})
}

var (
	okResp  = map[string]any{"output": "8"}
	errResp = map[string]any{"error": "requires confirmation"}
)

// message builds a message with the role and ID given, and parts.
func messageOf(role a2a.MessageRole, id string, parts ...*a2a.Part) *a2a.Message {
	return &a2a.Message{ID: id, Role: role, Parts: parts}
}

// statusOf is a status update in state whose message is m.
func statusOf(state a2a.TaskState, m *a2a.Message) a2a.Event {
	return &a2a.TaskStatusUpdateEvent{Status: a2a.TaskStatus{State: state, Message: m}}
}

// taskOf is a task snapshot in state, with message m as its status message and artifacts arts.
func taskOf(state a2a.TaskState, m *a2a.Message, arts ...*a2a.Artifact) *a2a.Task {
	return &a2a.Task{ID: "t", Status: a2a.TaskStatus{State: state, Message: m}, Artifacts: arts}
}

func artifactOf(id string, parts ...*a2a.Part) *a2a.Artifact {
	return &a2a.Artifact{ID: a2a.ArtifactID(id), Parts: parts}
}

// outcomes lists the tool events as "call c1" and "result c1 <response>".
func outcomes(evs []Event) []string {
	var out []string
	for _, e := range evs {
		switch e := e.(type) {
		case ToolCallEvent:
			out = append(out, "call "+e.CallID)
		case ToolResultEvent:
			b, _ := json.Marshal(e.Response)
			out = append(out, fmt.Sprintf("result %s %s", e.CallID, b))
		}
	}
	return out
}

const (
	okJSON  = `{"output":"8"}`
	errJSON = `{"error":"requires confirmation"}`
)

// Approving a call (kagent 1.0, test/fixtures/kagent/approval/02-approve): the live stream carries the real result,
// and the final task replays the paused turn's call and its "requires confirmation" error as well. The approved call
// is one call and its real result, in that order: the stale error is history, and must not end the call as failed.
func TestAnApprovedCallEndsWithItsRealResult(t *testing.T) {
	events := fixtureEvents(t, "approval/02-approve.response.sse")
	_, got := run(events)
	want := []string{"call call_1", `result call_1 {"output":"8"}`}
	if got := outcomes(got); !reflect.DeepEqual(got, want) {
		t.Errorf("tool events = %q, want %q", got, want)
	}
	// The same task found again after a restart (Follow's GetTask): there is no live stream to lean on, and the
	// snapshot holds the error and then the real result, of which the later one is the outcome.
	_, got = run(events[len(events)-1:])
	if got := outcomes(got); !reflect.DeepEqual(got, want) {
		t.Errorf("tool events from the final task alone = %q, want %q", got, want)
	}
}

// The turn that paused still reports its call and its error: nothing newer exists.
func TestAPausedCallEndsWithItsError(t *testing.T) {
	_, got := run(fixtureEvents(t, "approval/01-pause-approve.response.sse"))
	want := []string{"call call_1", `result call_1 {"error":"error tool \"add_numbers\" requires confirmation, please approve or reject"}`}
	if got := outcomes(got); !reflect.DeepEqual(got, want) {
		t.Errorf("tool events = %q, want %q", got, want)
	}
}

// A result never reaches the consumer before its call, which a step needs: a result that comes first waits for the
// call, and is delivered right after it. One whose call never comes is delivered when the send ends.
func TestAResultWaitsForItsCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"call after result", []a2a.Event{toolArtifact("a1", resultPart(kagv1, "c1", okResp)), toolArtifact("a2", callPart(kagv1, "c1"))}, []string{"call c1", "result c1 " + okJSON}},
		{"both in one message, result first", []a2a.Event{agentStatus("m1", resultPart(v09, "c1", okResp), callPart(v09, "c1"))}, []string{"call c1", "result c1 " + okJSON}},
		{"call and result in order", []a2a.Event{toolArtifact("a1", callPart(kagv1, "c1")), toolArtifact("a2", resultPart(kagv1, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"a result with no ID is not held", []a2a.Event{toolArtifact("a1", resultPart(kagv1, "", okResp))}, []string{"result  " + okJSON}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
	t.Run("a result whose call never comes is delivered before the final event", func(t *testing.T) {
		_, got := run([]a2a.Event{toolArtifact("a1", resultPart(kagv1, "c1", okResp)), taskOf(a2a.TaskStateCompleted, nil)})
		if len(got) < 2 {
			t.Fatalf("events = %#v", got)
		}
		if _, isResult := got[len(got)-2].(ToolResultEvent); !isResult {
			t.Errorf("events = %#v, want the result just before the final event", got)
		}
		if _, isFinal := got[len(got)-1].(FinalEvent); !isFinal {
			t.Errorf("last event = %#v, want the final event", got[len(got)-1])
		}
	})
}

// A task snapshot is history: a result it holds for a call whose result the live stream delivered already is stale
// and is not delivered, and of several results in one snapshot for one call the last is the outcome.
func TestASnapshotNeverOverridesTheLiveResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"a live result, then a snapshot with an older one", []a2a.Event{
			toolArtifact("a1", callPart(kagv1, "c1")), toolArtifact("a3", resultPart(kagv1, "c1", okResp)),
			taskOf(a2a.TaskStateCompleted, nil, artifactOf("a1", callPart(kagv1, "c1")), artifactOf("a2", resultPart(kagv1, "c1", errResp)), artifactOf("a3", resultPart(kagv1, "c1", okResp)))},
			[]string{"call c1", "result c1 " + okJSON}},
		{"a snapshot alone: the last result of a call wins", []a2a.Event{
			taskOf(a2a.TaskStateCompleted, nil, artifactOf("a1", callPart(kagv1, "c1")), artifactOf("a2", resultPart(kagv1, "c1", errResp)), artifactOf("a3", resultPart(kagv1, "c1", okResp)))},
			[]string{"call c1", "result c1 " + okJSON}},
		{"results of other calls are not superseded", []a2a.Event{
			taskOf(a2a.TaskStateCompleted, nil, artifactOf("a1", callPart(kagv1, "c1"), callPart(kagv1, "c2")), artifactOf("a2", resultPart(kagv1, "c1", errResp)), artifactOf("a3", resultPart(kagv1, "c2", okResp)))},
			[]string{"call c1", "call c2", "result c1 " + errJSON, "result c2 " + okJSON}},
		// The snapshot lags the stream: it holds the replayed call and the stale error but not the real result,
		// which streamed. Last-wins within the snapshot cannot tell, so the live result alone keeps the error out.
		{"a snapshot that lags: the live result stands, the stale error is not delivered", []a2a.Event{
			toolArtifact("a3", resultPart(kagv1, "c1", okResp)),
			taskOf(a2a.TaskStateCompleted, nil, artifactOf("a1", callPart(kagv1, "c1")), artifactOf("a2", resultPart(kagv1, "c1", errResp)))},
			[]string{"call c1", "result c1 " + okJSON}},
		// A live result of one source does not make a snapshot's result of the other source stale: what the
		// other source says is another part, which may carry the real outcome.
		{"a live result in a message, a different one in the snapshot's artifacts", []a2a.Event{
			agentStatus("m1", callPart(adk, "c1"), resultPart(adk, "c1", okResp)),
			taskOf(a2a.TaskStateCompleted, nil, artifactOf("a1", resultPart(adk, "c1", errResp)))},
			[]string{"call c1", "result c1 " + okJSON, "result c1 " + errJSON}},
		// Two live results are the agent's own sequence, and are delivered as they came.
		{"two live results are both delivered", []a2a.Event{
			toolArtifact("a1", callPart(kagv1, "c1")), toolArtifact("a2", resultPart(kagv1, "c1", errResp)), toolArtifact("a3", resultPart(kagv1, "c1", okResp))},
			[]string{"call c1", "result c1 " + errJSON, "result c1 " + okJSON}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// Only what the agent says as the task works is progress: an agent-role status message in state working. The user's
// own message echoed back, and the message of a pause, are not.
func TestOnlyWorkingAgentStatusMessagesAreRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state a2a.TaskState
		role  a2a.MessageRole
		want  []string
	}{
		{"agent, working", a2a.TaskStateWorking, a2a.MessageRoleAgent, []string{"call c1"}},
		{"user echo, working", a2a.TaskStateWorking, a2a.MessageRoleUser, nil},
		{"no role, working", a2a.TaskStateWorking, a2a.MessageRoleUnspecified, nil},
		{"agent, input required", a2a.TaskStateInputRequired, a2a.MessageRoleAgent, nil},
		{"agent, auth required", a2a.TaskStateAuthRequired, a2a.MessageRoleAgent, nil},
		{"agent, submitted", a2a.TaskStateSubmitted, a2a.MessageRoleAgent, nil},
		{"agent, completed", a2a.TaskStateCompleted, a2a.MessageRoleAgent, nil},
		{"agent, failed", a2a.TaskStateFailed, a2a.MessageRoleAgent, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := messageOf(tc.role, "m1", callPart(adk, "c1"))
			if _, got := run([]a2a.Event{statusOf(tc.state, msg)}); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("update: tool events = %q, want %q", outcomes(got), tc.want)
			}
			if _, got := run([]a2a.Event{taskOf(tc.state, msg)}); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("snapshot: tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// A task snapshot's status message is read as a status update's is, and a part both hold is delivered once.
func TestASnapshotsStatusMessageIsReadOnce(t *testing.T) {
	call := messageOf(a2a.MessageRoleAgent, "m1", callPart(v09, "c1"))
	idless := messageOf(a2a.MessageRoleAgent, "", callPart(v09, "c1"))
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"a snapshot alone, as GetTask returns it", []a2a.Event{taskOf(a2a.TaskStateWorking, call)}, []string{"call c1"}},
		{"after the live update that held it", []a2a.Event{statusOf(a2a.TaskStateWorking, call), taskOf(a2a.TaskStateWorking, call)}, []string{"call c1"}},
		{"before the live update that holds it", []a2a.Event{taskOf(a2a.TaskStateWorking, call), statusOf(a2a.TaskStateWorking, call)}, []string{"call c1"}},
		{"the same message with no ID, live and in the snapshot", []a2a.Event{statusOf(a2a.TaskStateWorking, idless), taskOf(a2a.TaskStateWorking, idless)}, []string{"call c1"}},
		{"a snapshot of a paused task", []a2a.Event{taskOf(a2a.TaskStateInputRequired, call)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// The same tool part found in an artifact and in a status message is one part. The part is what it says: a part of
// the same kind and call ID that says something else is a different part, which may carry the real outcome, and a
// part with no call ID is what it says alone.
func TestToolPartsAreTheSamePartAcrossSourcesWhenTheySaySoTheSame(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"a result in an artifact, then the same one in a message", []a2a.Event{toolArtifact("a1", callPart(adk, "c1"), resultPart(adk, "c1", okResp)), agentStatus("m1", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"a result in a message, then the same one in an artifact", []a2a.Event{agentStatus("m1", callPart(adk, "c1"), resultPart(adk, "c1", okResp)), toolArtifact("a1", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"a result in an artifact, then another for the call in a message", []a2a.Event{toolArtifact("a1", callPart(adk, "c1"), resultPart(adk, "c1", errResp)), agentStatus("m1", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + errJSON, "result c1 " + okJSON}},
		{"a result in a message, then another for the call in an artifact", []a2a.Event{agentStatus("m1", callPart(adk, "c1"), resultPart(adk, "c1", errResp)), toolArtifact("a1", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + errJSON, "result c1 " + okJSON}},
		{"a call with no ID in an artifact and in a message", []a2a.Event{toolArtifact("a1", callPart(adk, "")), agentStatus("m1", callPart(adk, ""))}, []string{"call "}},
		{"a call with no ID in a message and in an artifact", []a2a.Event{agentStatus("m1", callPart(adk, "")), toolArtifact("a1", callPart(adk, ""))}, []string{"call "}},
		{"a result with no ID in an artifact and in a message", []a2a.Event{toolArtifact("a1", resultPart(adk, "", okResp)), agentStatus("m1", resultPart(adk, "", okResp))}, []string{"result  " + okJSON}},
		{"calls with no ID that differ", []a2a.Event{toolArtifact("a1", callPart(adk, "")), agentStatus("m1", markedPart(adk, "function_call", map[string]any{"name": "other"}))}, []string{"call ", "call "}},
		{"calls with no ID in two artifacts are two calls", []a2a.Event{toolArtifact("a1", callPart(adk, "")), toolArtifact("a2", callPart(adk, ""))}, []string{"call ", "call "}},
		{"a call of the same ID in an artifact and in a message", []a2a.Event{toolArtifact("a1", callPart(adk, "c1")), agentStatus("m1", callPart(adk, "c1"))}, []string{"call c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// A status message with no ID has no place of its own: two of them are two messages, unless they say the same.
func TestStatusMessagesWithNoIDAreNotOnePlace(t *testing.T) {
	c1, c2 := callPart(v09, "c1"), callPart(v09, "c2")
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"two messages with different calls", []a2a.Event{agentStatus("", c1), agentStatus("", c2)}, []string{"call c1", "call c2"}},
		{"a call and its result", []a2a.Event{agentStatus("", c1), agentStatus("", resultPart(v09, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"one message delivered twice", []a2a.Event{agentStatus("", c1), agentStatus("", c1)}, []string{"call c1"}},
		{"a message with an ID and one without, of different calls", []a2a.Event{agentStatus("m1", c1), agentStatus("", c2)}, []string{"call c1", "call c2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// ADK's A2A converters put a tool call in a status message (the older one) or in an artifact (the newer one), and mark
// it adk_type. Both placements read the same, on any dialect, and ADK's other part types stay opaque.
func TestADKToolPartsInBothPlacements(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"status messages", []a2a.Event{agentStatus("m1", callPart(adk, "c1")), agentStatus("m2", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"artifacts", []a2a.Event{toolArtifact("a1", callPart(adk, "c1")), toolArtifact("a2", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"a call in a message and its result in an artifact", []a2a.Event{agentStatus("m1", callPart(adk, "c1")), toolArtifact("a1", resultPart(adk, "c1", okResp))}, []string{"call c1", "result c1 " + okJSON}},
		{"other ADK types in a message", []a2a.Event{agentStatus("m1",
			markedPart(adk, "tool_call", map[string]any{"id": "c1", "name": "get"}), markedPart(adk, "tool_response", map[string]any{"id": "c1", "name": "get"}),
			markedPart(adk, "executable_code", map[string]any{"code": "1", "language": "PYTHON"}), markedPart(adk, "code_execution_result", map[string]any{"outcome": "OUTCOME_OK", "output": "1"}))}, nil},
		{"other ADK types in an artifact", []a2a.Event{toolArtifact("a1", markedPart(adk, "tool_call", map[string]any{"id": "c1", "name": "get"}))}, nil},
		{"a marked part with no name", []a2a.Event{agentStatus("m1", markedPart(adk, "function_call", map[string]any{"id": "c1"}))}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(outcomes(got), tc.want) {
				t.Errorf("tool events = %q, want %q", outcomes(got), tc.want)
			}
		})
	}
}

// Every dialect reads tool parts, so the generic one shows what a kagent 0.9.x agent does in a turn over A2A 0.3: reached
// through each dialect's own connect (the 0.3 card, the 0.3 methods), a recorded turn yields the call and its result,
// in that order, then the answer.
func TestEveryDialectShowsToolStepsOfAKagent09Turn(t *testing.T) {
	for name, d := range map[string]dialect.Dialect{"a2a": generic.New(0), "kagent-v1": kagentv1.New(0)} {
		t.Run(name, func(t *testing.T) {
			fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
				if call.Method == "tasks/get" {
					reply(t, w, "v09-tools/02-gettask-after-tool-turn.response.json")
					return
				}
				sse(t, w, "v09-tools/01-tool-turn.response.sse", -1)
			})
			fa.v03 = true
			h, got := collector()
			if err := fa.agentVia(t, d).Send(t.Context(), message("How many namespaces?"), h); err != nil {
				t.Fatal(err)
			}
			want := []string{"call k8s_get_resources_0_1e73ff4e"}
			if g := outcomes(*got); len(g) != 2 || g[0] != want[0] || !strings.HasPrefix(g[1], "result k8s_get_resources_0_1e73ff4e ") {
				t.Errorf("tool events = %q, want the call, then its result", g)
			}
		})
	}
}
