package stubagent_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const adkMeta = "https://google.github.io/adk-docs/a2a/a2a-extension/"

func TestSendStreamingMessageOpensLikeKagent(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()

	evs := c.send(msg("m1", "What is 2+2?", ""))

	rec := fixture(t, "session/01-new-session.response.sse")
	if got, want := shape(evs[:3]), shape(rec[:3]); got != want {
		t.Fatalf("opening = %s, want %s", got, want)
	}
	for i := range 3 {
		expectLike(t, describe(evs[i]), evs[i], rec[i])
	}
	// Two SUBMITTED tasks open the stream; only the second carries ADK's marker.
	if dig(evs[0], "result", "task", "metadata", adkMeta) != nil || dig(evs[1], "result", "task", "metadata", adkMeta) != true {
		t.Errorf("want the ADK marker on the second SUBMITTED task only")
	}
	if _, ctxID, state := ids(last(evs)); describe(last(evs)) != "task:COMPLETED" || ctxID == "" || state != completed {
		t.Errorf("last event = %s, want the COMPLETED task", describe(last(evs)))
	}
}

func TestStreamIsServerSentEvents(t *testing.T) {
	s, agent, _ := newAgent(t)
	agent.Next()
	req := obj{"jsonrpc": "2.0", "id": "7", "method": "SendStreamingMessage", "params": msg("m1", "hi", "")}

	_, raw := do(t, http.MethodPost, s.AgentURL("kagent", "test"), req, http.Header{})

	events := strings.Split(strings.TrimSpace(string(raw)), "\n\n")
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4:\n%s", len(events), raw)
	}
	for _, e := range events {
		id, data, ok := strings.Cut(e, "\n")
		if !strings.HasPrefix(id, "id: ") || !strings.HasPrefix(data, `data: {"id":"7","jsonrpc":"2.0","result":`) || !ok {
			t.Errorf("event %q, want an id line then a data line with the request id 7", e)
		}
	}
}

func TestStreamingErrorIsOneSSEEvent(t *testing.T) {
	_, _, c := newAgent(t)

	st := c.open("SendStreamingMessage", msg("m1", "hi", ""))
	evs := st.all()

	if !st.sse || st.status != http.StatusOK || len(evs) != 1 || dig(evs[0], "error", "code") != float64(-32603) {
		t.Errorf("unscripted streaming send = HTTP %d, sse %v, %v; want one error event", st.status, st.sse, evs)
	}
}

func TestReplyScriptsTurnsByPrompt(t *testing.T) {
	_, agent, c := newAgent(t)
	var prompts []string
	agent.Reply(func(prompt string) []stubagent.Step {
		prompts = append(prompts, prompt)
		return nil
	})
	agent.Next()

	c.send(msg("m1", "queued first", ""))
	c.send(msg("m2", "then the reply", ""))

	if len(prompts) != 1 || prompts[0] != "then the reply" {
		t.Errorf("Reply saw %q, want only the message that found no queued turn", prompts)
	}
}
