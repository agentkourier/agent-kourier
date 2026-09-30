package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
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

// run feeds events to a fresh assembler and returns what it emitted.
func run(evs ...[]a2a.Event) (*assembler, []Event) {
	var got []Event
	asm := newAssembler(func(e Event) { got = append(got, e) })
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
