package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
)

const ticks = "tick 1 tick 2 tick 3 tick 4 tick 5 tick 6 tick 7 tick 8 "

// fixturePath is a file under test/fixtures/kagent, recorded from a real agent (WP-02).
func fixturePath(name string) string {
	return filepath.Join("..", "..", "test", "fixtures", "kagent", filepath.FromSlash(name))
}

// fixtureEvents parses the events of a recorded SSE stream.
func fixtureEvents(t *testing.T, name string) []a2a.Event {
	t.Helper()
	body, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}
	var evs []a2a.Event
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var env struct {
			Result a2a.StreamResponse `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		evs = append(evs, env.Result.Event)
	}
	return evs
}

// run feeds events to a fresh assembler that knows kagent's tool parts.
func run(evs ...[]a2a.Event) (*assembler, []Event) { return runWith(kagentv1.New(0).ToolPart, evs...) }

// runWith feeds events to a fresh assembler and returns what it emitted.
func runWith(classify ToolClassifier, evs ...[]a2a.Event) (*assembler, []Event) {
	var got []Event
	asm := newAssembler(func(e Event) { got = append(got, e) }, classify)
	for _, batch := range evs {
		for _, ev := range batch {
			asm.apply(ev)
		}
	}
	return asm, got
}

// texts returns the accumulated text of each TextEvent.
func texts(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if te, ok := e.(TextEvent); ok {
			out = append(out, te.Text)
		}
	}
	return out
}

func states(evs []Event) []a2a.TaskState {
	var out []a2a.TaskState
	for _, e := range evs {
		if se, ok := e.(StateEvent); ok {
			out = append(out, se.State)
		}
	}
	return out
}

func chunk(id, text string, appendMode bool) a2a.Event {
	return &a2a.TaskArtifactUpdateEvent{Append: appendMode, Artifact: &a2a.Artifact{ID: a2a.ArtifactID(id), Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}
}

func TestLiveStreamAssemblesToFinalText(t *testing.T) {
	asm, got := run(fixtureEvents(t, "livestream/01-slow-stream.response.sse"))
	want := []string{"tick 1 ", "tick 1 tick 2 ", "tick 1 tick 2 tick 3 ", "tick 1 tick 2 tick 3 tick 4 ",
		"tick 1 tick 2 tick 3 tick 4 tick 5 ", "tick 1 tick 2 tick 3 tick 4 tick 5 tick 6 ",
		"tick 1 tick 2 tick 3 tick 4 tick 5 tick 6 tick 7 ", ticks}
	if got := texts(got); !reflect.DeepEqual(got, want) {
		t.Errorf("texts = %q\nwant %q (the lastChunk replace must not emit a duplicate)", got, want)
	}
	if got, want := states(got), []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted}; !reflect.DeepEqual(got, want) {
		t.Errorf("states = %v, want %v (the second SUBMITTED is not a change)", got, want)
	}
	final, ok := got[len(got)-1].(FinalEvent)
	if !ok || final.Task.Status.State != a2a.TaskStateCompleted || asm.final != final.Task {
		t.Fatalf("last event = %#v, want the COMPLETED task", got[len(got)-1])
	}
	if art := final.Task.Artifacts[0]; art.Parts[0].Text() != ticks {
		t.Errorf("final task text = %q, want %q", art.Parts[0].Text(), ticks)
	}
}

func TestAppendAndReplacePerArtifact(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"append adds", []a2a.Event{chunk("a", "one ", false), chunk("a", "two", true)}, []string{"one ", "one two"}},
		{"no flag replaces", []a2a.Event{chunk("a", "one ", false), chunk("a", "new", false)}, []string{"one ", "new"}},
		{"lastChunk carries the full text", []a2a.Event{chunk("a", "one ", false), chunk("a", "two", true), &a2a.TaskArtifactUpdateEvent{LastChunk: true, Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("one two")}}}}, []string{"one ", "one two"}},
		{"artifacts are independent", []a2a.Event{chunk("a", "A", false), chunk("b", "B", false), chunk("a", "2", true)}, []string{"A", "B", "A2"}},
		{"a live replace may shorten the text", []a2a.Event{chunk("a", "abc", false), chunk("a", "a", false)}, []string{"abc", "a"}},
		{"a live replace may empty the text", []a2a.Event{chunk("a", "abc", false), chunk("a", "", false)}, []string{"abc", ""}},
		{"an update with no text part leaves the text as it was", []a2a.Event{chunk("a", "abc", false),
			&a2a.TaskArtifactUpdateEvent{Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewDataPart(map[string]any{"k": "v"})}}}}, []string{"abc"}},
		{"an empty append changes nothing", []a2a.Event{chunk("a", "A", false), chunk("a", "", true)}, []string{"A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := run(tc.evs); !reflect.DeepEqual(texts(got), tc.want) {
				t.Errorf("texts = %q, want %q", texts(got), tc.want)
			}
		})
	}
}

// Reconnect: the stream drops after its first tick, and a snapshot from SubscribeToTask
// starts the rest. The snapshot replaces the text; the deltas append to it.
func TestSnapshotThenDeltasHasNoDuplicateText(t *testing.T) {
	dropped := fixtureEvents(t, "reconnect/01-stream-dropped.response.sse")
	resumed := fixtureEvents(t, "reconnect/03-subscribe.response.sse")
	_, got := run(dropped, resumed)
	seen := map[string]bool{}
	for _, s := range texts(got) {
		if seen[s] {
			t.Errorf("text %q delivered twice", s)
		}
		seen[s] = true
	}
	if all := texts(got); all[len(all)-1] != ticks {
		t.Errorf("final text = %q, want %q", all[len(all)-1], ticks)
	}
}

// An in-flight replay of the same messageId opens like SubscribeToTask (spec 8.2).
func TestReplayOfRunningSendJoinsTheLiveStream(t *testing.T) {
	asm, got := run(fixtureEvents(t, "inflight/02-a-replay-while-running.response.sse"))
	if all := texts(got); all[0] != "tick 1 " || all[len(all)-1] != ticks {
		t.Errorf("texts = %q, want from the snapshot's %q to %q", all, "tick 1 ", ticks)
	}
	if asm.final == nil || asm.taskID == "" || asm.contextID == "" {
		t.Errorf("final = %v, task = %q, context = %q; want all set", asm.final, asm.taskID, asm.contextID)
	}
}

func TestToolArtifactsAreDeliveredOnce(t *testing.T) {
	_, got := run(fixtureEvents(t, "approval/01-pause-approve.response.sse"))
	var calls []ToolCallEvent
	var results []ToolResultEvent
	for _, e := range got {
		switch e := e.(type) {
		case ToolCallEvent:
			calls = append(calls, e)
		case ToolResultEvent:
			results = append(results, e)
		}
	}
	if len(calls) != 1 || calls[0].Name != "add_numbers" || calls[0].CallID != "call_1" || calls[0].Args["a"] != float64(3) {
		t.Errorf("calls = %+v, want one add_numbers(a=3, b=5); the final task repeats its artifacts and must not deliver them again", calls)
	}
	if len(results) != 1 || results[0].CallID != "call_1" || results[0].Response == nil {
		t.Errorf("results = %+v, want one result for call_1", results)
	}
	if final, ok := got[len(got)-1].(FinalEvent); !ok || final.Task.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("last event = %#v, want the INPUT_REQUIRED task", got[len(got)-1])
	}
}

func TestSnapshotOfFinishedTaskDeliversItsText(t *testing.T) {
	_, got := run(fixtureEvents(t, "session/02-replay-same-messageid.response.sse"))
	if want := []string{"The answer is 4."}; !reflect.DeepEqual(texts(got), want) {
		t.Errorf("texts = %q, want %q", texts(got), want)
	}
	if _, ok := got[len(got)-1].(FinalEvent); !ok {
		t.Errorf("last event = %#v, want FinalEvent", got[len(got)-1])
	}
}

func snapshot(text string) *a2a.Task {
	return &a2a.Task{ID: "t", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}}
}

// kagent never shortens an artifact's text within a turn, so a snapshot that is a strict
// prefix of what we hold is older than it (a GetTask that lags the stream) and is skipped.
func TestSnapshotThatLagsDoesNotShortenTheText(t *testing.T) {
	deltas := []a2a.Event{chunk("a", "a", false), chunk("a", "b", true)}
	_, got := run(append(deltas, snapshot("a")))
	if want := []string{"a", "ab"}; !reflect.DeepEqual(texts(got), want) {
		t.Errorf("texts = %q, want %q: the snapshot \"a\" is older than \"ab\"", texts(got), want)
	}
	_, got = run(append(deltas, snapshot("x")))
	if want := []string{"a", "ab", "x"}; !reflect.DeepEqual(texts(got), want) {
		t.Errorf("texts = %q, want %q: a snapshot that is not a prefix replaces the text", texts(got), want)
	}
}

func TestOnlyOneFinalEvent(t *testing.T) {
	done := &a2a.Task{ID: "t", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	_, got := run([]a2a.Event{done, done})
	finals := 0
	for _, e := range got {
		if _, ok := e.(FinalEvent); ok {
			finals++
		}
	}
	if finals != 1 {
		t.Errorf("%d FinalEvents, want 1", finals)
	}
}

// Without a classifier a dialect has said nothing about data parts, so they stay opaque.
func TestToolPartsAreOpaqueWithoutAClassifier(t *testing.T) {
	_, got := runWith(nil, fixtureEvents(t, "approval/01-pause-approve.response.sse"))
	for _, e := range got {
		switch e.(type) {
		case ToolCallEvent, ToolResultEvent:
			t.Errorf("event %#v from an assembler with no classifier", e)
		}
	}
	if _, ok := got[len(got)-1].(FinalEvent); !ok {
		t.Errorf("last event = %#v, want FinalEvent", got[len(got)-1])
	}
}

// kagent 0.9.x sends a turn's tool calls and results only as the messages of its status updates,
// marked kagent_type, and its final text both as a status message and as an artifact
// (recorded from kagent 0.9.4 over A2A 0.3). The task that ends the turn holds the tool parts in its
// history, which is not read: they were delivered as they happened.
func TestV09ToolPartsInStatusMessagesAreDelivered(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "tasks/get" { // the stream ends with a status update, not a task, so the send asks for it
			reply(t, w, "v09-tools/02-gettask-after-tool-turn.response.json")
			return
		}
		sse(t, w, "v09-tools/01-tool-turn.response.sse", -1)
	})
	h, got := collector()
	if err := fa.agentV03(t).Send(t.Context(), message("How many namespaces does the cluster have?"), h); err != nil {
		t.Fatal(err)
	}
	const callID = "k8s_get_resources_0_1e73ff4e"
	var order []string
	for _, e := range *got {
		switch e := e.(type) {
		case ToolCallEvent:
			order = append(order, "call "+e.Name)
			if e.CallID != callID || e.Args["resource_type"] != "namespace" {
				t.Errorf("call = %+v, want %s(resource_type=namespace)", e, callID)
			}
		case ToolResultEvent:
			order = append(order, "result "+e.Name)
			if e.CallID != callID || e.Response == nil {
				t.Errorf("result = %+v, want a response for %s", e, callID)
			}
		case TextEvent:
			order = append(order, "text "+e.Text)
		}
	}
	want := []string{"call k8s_get_resources", "result k8s_get_resources", "text The cluster has 46 namespaces."}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("events = %q\nwant %q (the status message that repeats the answer is not a second text)", order, want)
	}
	if got, want := states(*got), []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted}; !reflect.DeepEqual(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
	if final, ok := (*got)[len(*got)-1].(FinalEvent); !ok || final.Task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("last event = %#v, want the COMPLETED task", (*got)[len(*got)-1])
	}
}

// v09Tool is a tool part as kagent 0.9.x marks it.
func v09Tool(partType string, data map[string]any) *a2a.Part {
	p := a2a.NewDataPart(data)
	p.Metadata = map[string]any{"kagent_type": partType}
	return p
}

// agentStatus is a WORKING status update whose message, from the agent, holds parts.
func agentStatus(msgID string, parts ...*a2a.Part) a2a.Event {
	return &a2a.TaskStatusUpdateEvent{Status: a2a.TaskStatus{State: a2a.TaskStateWorking,
		Message: &a2a.Message{ID: msgID, Role: a2a.MessageRoleAgent, Parts: parts}}}
}

func toolArtifact(id string, parts ...*a2a.Part) a2a.Event {
	return &a2a.TaskArtifactUpdateEvent{Artifact: &a2a.Artifact{ID: a2a.ArtifactID(id), Parts: parts}}
}

// tools returns "call <id>" or "result <id>" for each tool event.
func tools(evs []Event) []string {
	var out []string
	for _, e := range evs {
		switch e := e.(type) {
		case ToolCallEvent:
			out = append(out, "call "+e.CallID)
		case ToolResultEvent:
			out = append(out, "result "+e.CallID)
		}
	}
	return out
}

func TestStatusMessageToolPartsAreDeliveredOnce(t *testing.T) {
	call := v09Tool("function_call", map[string]any{"id": "c1", "name": "get", "args": map[string]any{}})
	result := v09Tool("function_response", map[string]any{"id": "c1", "name": "get", "response": map[string]any{"ok": true}})
	for _, tc := range []struct {
		name string
		evs  []a2a.Event
		want []string
	}{
		{"a call then its result", []a2a.Event{agentStatus("m1", call), agentStatus("m2", result)}, []string{"call c1", "result c1"}},
		{"a repeated message", []a2a.Event{agentStatus("m1", call), agentStatus("m1", call), agentStatus("m2", result)}, []string{"call c1", "result c1"}},
		{"one message holding both", []a2a.Event{agentStatus("m1", call, result)}, []string{"call c1", "result c1"}},
		{"an artifact, then a message repeating it", []a2a.Event{toolArtifact("a1", call), agentStatus("m1", call)}, []string{"call c1"}},
		{"a message, then an artifact repeating it", []a2a.Event{agentStatus("m1", call), toolArtifact("a1", call)}, []string{"call c1"}},
		// kagent 1.0 answers an approved call with a second result under the same ID (approval/02-approve).
		{"two results for one call in artifacts", []a2a.Event{toolArtifact("a1", result), toolArtifact("a2", result)}, []string{"result c1", "result c1"}},
		{"a message with no tool part", []a2a.Event{agentStatus("m1", a2a.NewTextPart("thinking"))}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := run(tc.evs)
			if got := tools(got); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tool events = %q, want %q", got, tc.want)
			}
			if got := texts(got); got != nil {
				t.Errorf("texts = %q, want none: a status message's text is not an artifact", got)
			}
		})
	}
}

func TestStatusMessageToolPartsAreOpaqueWithoutAClassifier(t *testing.T) {
	call := v09Tool("function_call", map[string]any{"id": "c1", "name": "get"})
	if _, got := runWith(nil, []a2a.Event{agentStatus("m1", call)}); tools(got) != nil {
		t.Errorf("tool events = %q from an assembler with no classifier", tools(got))
	}
}
