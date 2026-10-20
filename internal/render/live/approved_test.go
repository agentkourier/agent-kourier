package live_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// recordedConn is an agent connection whose stream is a recorded kagent response (test/fixtures/kagent), so the
// events the renderer gets are the ones the agent client makes of that stream, and not ones a test composes.
type recordedConn struct {
	agent.Conn
	events []a2a.Event
}

func (c recordedConn) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, ev := range c.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// recordedEvents parses the events of a recorded A2A 1.0 SSE stream.
func recordedEvents(t *testing.T, name string) []a2a.Event {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "kagent", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	var events []a2a.Event
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
		events = append(events, env.Result.Event)
	}
	return events
}

// The turn that approves a call (kagent 1.0, approval/02-approve) streams the call's real result, and its final
// task replays the paused turn's call and its "requires confirmation" error. The agent client delivers the call,
// then the real result, so the call is one step, and it ends done, not failed and not closed with no result.
func TestAnApprovedCallIsOneStepThatEndsDone(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	conn := recordedConn{events: recordedEvents(t, "approval/02-approve.response.sse")}
	a := agent.New(func(context.Context) (agent.Conn, error) { return conn, nil }, agent.Config{})
	if err := a.Answer(t.Context(), a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("yes")), run.Handle); err != nil {
		t.Fatal(err)
	}
	e.finish(t, run)
	m := e.reply()
	if got := m.Tasks(); len(got) != 1 || got[0].Title != "add_numbers" || got[0].Status != "complete" {
		t.Fatalf("tasks %+v; want one add_numbers step, complete", got)
	}
	// A step that was left with no result is closed as complete too, with a note that none was reported (platform.go,
	// noResult): the status alone does not tell it from done. This one had its result, so it has no such note.
	if got := m.Tasks()[0]; got.Output != "" {
		t.Fatalf("task %+v; want the step done by the call's result, not closed with a note at the end", got)
	}
	if m.Text != "I have added 3 and 5. The result is 8." {
		t.Fatalf("text %q; want the answer", m.Text)
	}
}
