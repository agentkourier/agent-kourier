package stubagent_test

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

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

func TestScenarioSlowAgentStreamsItsTicks(t *testing.T) {
	var want strings.Builder
	for n := 1; n <= stubagent.ScenarioSlowTicks; n++ {
		want.WriteString("tick " + strconv.Itoa(n) + " ")
	}
	if got := taskText(last(scenario(t, "scenario-slow").send(msg("m1", "go slowly", "")))); got != want.String() {
		t.Errorf("scenario-slow said %q, want %d ticks", got, stubagent.ScenarioSlowTicks)
	}
}
