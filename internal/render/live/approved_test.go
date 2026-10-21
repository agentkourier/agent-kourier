package live_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/render/live"
)

// recordedAgent is an Agent over conn that reads pauses as the kagent-v1 dialect does, as a Pool's Agent does.
func recordedAgent(conn recordedConn) *agent.Agent {
	return agent.New(func(context.Context) (agent.Conn, error) { return conn, nil }, agent.Config{ParseInteraction: kagentv1.New(0).ParseInteraction})
}

// recordedConn is an agent connection whose stream is a recorded kagent response (test/fixtures/kagent), so the
// events the renderer gets are the ones the agent client makes of that stream, and not ones a test composes.
type recordedConn struct {
	agent.Conn
	events []a2a.Event
	paused *a2a.Task // what GetTask returns: the task of the turn that paused, as the agent holds it while the answer runs
}

func (c recordedConn) GetTask(context.Context, *a2a.GetTaskRequest) (*a2a.Task, error) {
	return c.paused, nil
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
	pause := recordedEvents(t, "approval/01-pause-approve.response.sse")
	conn := recordedConn{events: recordedEvents(t, "approval/02-approve.response.sse"), paused: pause[len(pause)-1].(*a2a.Task)}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("yes"))
	msg.TaskID = conn.paused.ID
	if err := recordedAgent(conn).Answer(t.Context(), msg, run.Handle); err != nil {
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
	// The step is begun before the answer, where the call was made, and not after it, where the final task's replay
	// of the call would put it.
	tl := timeline(m)
	step, answer := slices.IndexFunc(tl, func(s string) bool { return strings.HasPrefix(s, "task:") }), slices.IndexFunc(tl, func(s string) bool { return strings.HasPrefix(s, "text:") })
	if step < 0 || answer < 0 || step > answer {
		t.Fatalf("chunks %v; want the step before the answer", tl)
	}
}

// The turn that answers a question (kagent 1.0, askuser/04-good-answer) streams the result of the ask_user call it paused
// on. The call is a step of the message of the turn that paused, which showed the question, and is not a step of the
// answer's message as well: one card for one question.
func TestAnAnsweredQuestionIsAStepOfThePauseAlone(t *testing.T) {
	e := setup(t)
	pause := recordedEvents(t, "askuser/03-pause-good-answer.response.sse")
	paused := pause[len(pause)-1].(*a2a.Task)

	asked := e.stream(t)
	if err := recordedAgent(recordedConn{events: pause}).Send(t.Context(), a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Which database?")), asked.Handle); err != nil {
		t.Fatal(err)
	}
	e.finish(t, asked)
	if got := e.reply().Tasks(); len(got) != 1 || got[0].Title != "ask_user" {
		t.Fatalf("tasks of the pause %+v; want the ask_user step", got)
	}

	answered := e.live.Start(t.Context(), live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Source: "turn-2", Channel: "C1", Thread: e.root})
	conn := recordedConn{events: recordedEvents(t, "askuser/04-good-answer.response.sse"), paused: paused}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("PostgreSQL"))
	msg.TaskID = paused.ID
	if err := recordedAgent(conn).Answer(t.Context(), msg, answered.Handle); err != nil {
		t.Fatal(err)
	}
	e.finish(t, answered)
	m := e.reply()
	if m.Text != "Using PostgreSQL for storage." {
		t.Fatalf("text %q; want the answer's", m.Text)
	}
	if got := m.Tasks(); len(got) != 0 {
		t.Fatalf("tasks of the answer %+v; want none: the ask_user step is the pause's", got)
	}
}
