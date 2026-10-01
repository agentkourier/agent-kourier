package stubagent_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// scenario starts a stub with the scenario agents and returns a client for one of them.
func scenario(t *testing.T, name string) *client {
	t.Helper()
	s := stubagent.New()
	t.Cleanup(s.Close)
	stubagent.ScenarioAgents(s)
	return withHITL(&client{t: t, url: s.AgentURL("kagent", name), hdr: http.Header{}})
}

func TestReplyWithHistorySeesTheContextsEarlierMessages(t *testing.T) {
	_, agent, c := newAgent(t)
	var priors [][]string
	agent.ReplyWithHistory(func(prior []string, _ string) []stubagent.Step {
		priors = append(priors, prior)
		return []stubagent.Step{stubagent.Text("ok")}
	})
	_, ctxID, _ := ids(last(c.send(msg("m1", "one", ""))))
	c.send(msg("m2", "two", ctxID))
	c.send(msg("m3", "three", ctxID))
	c.send(msg("m4", "elsewhere", "")) // a new context sees none of the first's messages

	want := [][]string{nil, {"one"}, {"one", "two"}, nil}
	if !reflect.DeepEqual(priors, want) {
		t.Errorf("earlier messages seen per turn = %q, want %q", priors, want)
	}
}

func TestScenarioMemoryAgentProvesTheContext(t *testing.T) {
	c := scenario(t, "scenario-memory")
	say := func(id, text, ctxID string) (reply, ctx string) {
		evs := c.send(msg(id, text, ctxID))
		_, ctx, _ = ids(last(evs))
		return taskText(last(evs)), ctx
	}

	got1, ctxID := say("m1", "first", "")
	got2, _ := say("m2", "second", ctxID)
	got3, _ := say("m3", "third", ctxID)
	// A turn sent without the contextId, as courier would after losing the thread's mapping, is
	// a new conversation, and says so.
	got4, _ := say("m4", "fourth", "")

	for i, tc := range []struct{ got, want string }{
		{got1, `Message 1. Earlier: none.`},
		{got2, `Message 2. Earlier: "first".`},
		{got3, `Message 3. Earlier: "first", "second".`},
		{got4, `Message 1. Earlier: none.`},
	} {
		if tc.got != tc.want {
			t.Errorf("turn %d said %q, want %q", i+1, tc.got, tc.want)
		}
	}
}

func TestReplyWithHistoryShowsAReplayThatRanTwice(t *testing.T) {
	_, agent, c := newAgent(t)
	var priors [][]string
	agent.ReplyWithHistory(func(prior []string, _ string) []stubagent.Step {
		priors = append(priors, prior)
		return []stubagent.Step{stubagent.Text("ok")}
	})
	_, ctxID, _ := ids(last(c.send(msg("m1", "one", ""))))
	c.send(msg("m1", "one", ctxID)) // courier's blind resend into the context, after the task ended
	c.send(msg("m2", "two", ctxID))

	if want := []string{"one", "one"}; !reflect.DeepEqual(priors[2], want) {
		t.Errorf("earlier messages at the third turn = %q, want the replayed turn twice, %q", priors[2], want)
	}
}

func TestScenarioAskEchoesWhatItReceived(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers [][]string
		want    string
	}{
		{"complete", [][]string{{"PostgreSQL", "MySQL"}, {"blue"}}, `Answers: "PostgreSQL, MySQL" | "blue".`},
		{"empty second entry", [][]string{{"PostgreSQL"}, {}}, `Answers: "PostgreSQL" | (none).`},
		{"empty first entry", [][]string{{}, {"blue"}}, `Answers: (none) | "blue".`},
		{"one entry", [][]string{{"PostgreSQL"}}, `Answers: "PostgreSQL".`},
	} {
		got := taskText(last(ask(t, scenario(t, "scenario-ask"), "anything", func(id string) obj { return answers(id, tc.answers...) })))
		if got != tc.want {
			t.Errorf("%s: scenario-ask said %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestScenarioAskAsksTheTwoQuestions(t *testing.T) {
	evs := scenario(t, "scenario-ask").send(msg("m1", "anything", ""))
	request, _ := dig(last(evs), "result", "task", "status", "message", "metadata", hitlURI).(obj)
	want := []any{
		obj{"question": "Which databases?", "choices": []any{"PostgreSQL", "MySQL"}, "multiple": true},
		obj{"question": "Favorite color?", "choices": []any{}, "multiple": false},
	}
	if !reflect.DeepEqual(request["questions"], want) {
		t.Errorf("questions = %v, want %v", request["questions"], want)
	}
}

func TestScenarioGatedAgentHoldsUntilReleased(t *testing.T) {
	s := stubagent.New()
	t.Cleanup(s.Close)
	stubagent.ScenarioAgents(s)
	c := withHITL(&client{t: t, url: s.AgentURL("kagent", "scenario-gated"), hdr: http.Header{}})

	st := c.open("SendStreamingMessage", msg("m1", "investigate gate:run-1", ""))
	var taskID, streamed string
	for !strings.Contains(streamed, "Holding") { // reading on would wait for the release
		ev, ok := st.next()
		if !ok {
			t.Fatalf("the stream ended after %q, before the first chunk", streamed)
		}
		if id, _, _ := ids(ev); id != "" {
			taskID = id
		}
		if kind, e := body(ev); kind == "artifactUpdate" {
			streamed += text(dig(e, "artifact", "parts"))
		}
	}
	if streamed != "Holding on gate run-1." {
		t.Fatalf("streamed %q before the gate, want the one chunk", streamed)
	}

	// The turn cannot end while the gate is shut, so waiting for it times out, on any machine.
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := s.WaitTurn(short, taskID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the turn ended before its gate was released: %v", err)
	}
	if _, got := c.call("GetTask", obj{"id": taskID}); describe(got) != "task:WORKING" {
		t.Errorf("held task is %s, want task:WORKING", describe(got))
	}

	s.Release("run-1")
	long, cancelLong := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelLong()
	if err := s.WaitTurn(long, taskID); err != nil {
		t.Fatalf("the turn did not end after its gate was released: %v", err)
	}
	evs := st.all()
	if got, want := taskText(last(evs)), "Holding on gate run-1. Released."; got != want {
		t.Errorf("final text %q, want %q", got, want)
	}
}

func TestScenarioGatedAgentDoesNotHoldWithoutAGate(t *testing.T) {
	c := scenario(t, "scenario-gated")
	if got := taskText(last(c.send(msg("m1", "investigate", "")))); got != "No gate named, not holding." {
		t.Errorf("said %q, want it to finish at once", got)
	}
}
