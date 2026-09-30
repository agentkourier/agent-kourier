package stubagent_test

import (
	"net/http"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// spike starts a stub with the spike's agents and returns a client for one of them.
func spike(t *testing.T, name string) *client {
	t.Helper()
	s := stubagent.New()
	t.Cleanup(s.Close)
	stubagent.SpikeAgents(s)
	return withHITL(&client{t: t, url: s.AgentURL("kagent", name), hdr: http.Header{}})
}

// ask sends a prompt, replies to the pause with payload, and returns the reply's stream.
func ask(t *testing.T, c *client, prompt string, payload func(requestID string) obj) []obj {
	t.Helper()
	evs := c.send(msg("m1", prompt, ""))
	taskID, ctxID, state := ids(last(evs))
	if state != inputRequired {
		t.Fatalf("%q ended %s, want %s", prompt, state, inputRequired)
	}
	request, _ := dig(last(evs), "result", "task", "status", "message", "metadata", hitlURI).(obj)
	id, _ := request["id"].(string)
	return c.send(reply("m2", "x", ctxID, taskID, payload(id)))
}

func TestSpikeAgentsAnswerLikeTheSpikesModels(t *testing.T) {
	sendText := func(agent, prompt string) string {
		return taskText(last(spike(t, agent).send(msg("m1", prompt, ""))))
	}
	if got := sendText("test-agent", "What is 2+2?"); got != "The answer is 4." {
		t.Errorf("test-agent said %q", got)
	}
	if got := sendText("spike-slow", "go slowly"); got != eightTicks {
		t.Errorf("spike-slow said %q, want eight ticks", got)
	}

	for _, tc := range []struct {
		name, prompt string
		payload      func(id string) obj
		want         string
	}{
		{"good answer", dbQuestion, func(id string) obj { return answers(id, []string{"PostgreSQL"}) }, "Using PostgreSQL for storage."},
		{"not a choice", dbQuestion, func(id string) obj { return answers(id, []string{"Oracle"}) }, "Using Oracle."},
		{"plain text", dbQuestion, func(string) obj { return nil }, "Saw a plain user message."},
		{"two answers", "Ask two questions", func(id string) obj { return answers(id, []string{"PostgreSQL", "MySQL"}, []string{"blue"}) }, "Both answered."},
	} {
		if got := taskText(last(ask(t, spike(t, "spike-ask"), tc.prompt, tc.payload))); got != tc.want {
			t.Errorf("spike-ask, %s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := taskText(last(ask(t, spike(t, "spike-nested"), "Ask the specialist", func(id string) obj { return answers(id, []string{"east"}) }))); got != "Region east chosen." {
		t.Errorf("spike-nested said %q", got)
	}
}

func TestSpikeApprovalAgent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload func(id string) obj
		want    string
	}{
		{"approve", func(id string) obj { return decisions(approval(id, true, "")) }, "I have added 3 and 5. The result is 8."},
		{"reject", func(id string) obj { return decisions(approval(id, false, "not today")) }, "Resumed after the tool result."},
		{"no decisions", func(string) obj { return decisions() }, "Saw a plain user message."},
	} {
		c := spike(t, "spike-approve")
		evs := c.send(msg("m1", "Use the tool to add 3 and 5", ""))
		taskID, ctxID, _ := ids(last(evs))
		request, _ := dig(last(evs), "result", "task", "status", "message", "metadata", hitlURI).(obj)

		got := c.send(reply("m2", "x", ctxID, taskID, tc.payload(toolIDs(request)[0])))

		if taskText(last(got)) != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, taskText(last(got)), tc.want)
		}
	}
}
