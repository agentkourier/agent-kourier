package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
)

// scripted is an adapter.Backend whose agent asks to run bash, then reports the outcome of the decision.
type scripted struct {
	mu      sync.Mutex
	streams map[string][]chan adapter.Event
	sent    []adapter.Outbound
	n       int
}

func (s *scripted) CreateSession(context.Context, string, map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("sesn_%d", s.n), nil
}

func (s *scripted) Sessions(context.Context) ([]adapter.SessionInfo, error) { return nil, nil }
func (s *scripted) Archive(context.Context, string) error                   { return nil }

func (s *scripted) Stream(ctx context.Context, id string) (adapter.EventStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan adapter.Event, 32)
	if s.streams == nil {
		s.streams = map[string][]chan adapter.Event{}
	}
	s.streams[id] = append(s.streams[id], ch)
	return &chanStream{ctx: ctx, ch: ch}, nil
}

func (s *scripted) Send(_ context.Context, id string, events ...adapter.Outbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range events {
		s.sent = append(s.sent, ev)
		var out []adapter.Event
		running := adapter.Event{Type: "session.status_running"}
		end := adapter.Event{Type: "session.status_idle", StopReason: &adapter.StopReason{Type: "end_turn"}}
		say := func(t string) adapter.Event {
			return adapter.Event{Type: "agent.message", Content: []adapter.ContentBlock{{Type: "text", Text: t}}}
		}
		switch {
		case ev.Message != nil && strings.Contains(*ev.Message, "bash"):
			input, _ := json.Marshal(map[string]any{"command": "hostname"})
			out = []adapter.Event{running, {ID: "sevt_ask", Type: "agent.tool_use", Name: "bash", Input: input, EvaluatedPermission: "ask"},
				{Type: "session.status_idle", StopReason: &adapter.StopReason{Type: "requires_action", EventIDs: []string{"sevt_ask"}}}}
		case ev.Message != nil:
			out = []adapter.Event{running, say("pong"), end}
		case ev.Confirmation != nil && ev.Confirmation.Allow:
			out = []adapter.Event{running, {Type: "agent.tool_result", ToolUseID: "sevt_ask", Content: []adapter.ContentBlock{{Type: "text", Text: "cma-sandbox-abc"}}}, say("The host is cma-sandbox-abc."), end}
		case ev.Confirmation != nil:
			out = []adapter.Event{running, say("I did not run it."), end}
		}
		for _, e := range out {
			for _, ch := range s.streams[id] {
				ch <- e
			}
		}
	}
	return nil
}

type chanStream struct {
	ctx context.Context
	ch  chan adapter.Event
	cur adapter.Event
}

func (c *chanStream) Next() bool {
	select {
	case e := <-c.ch:
		c.cur = e
		return true
	case <-c.ctx.Done():
		return false
	}
}
func (c *chanStream) Current() adapter.Event { return c.cur }
func (c *chanStream) Err() error             { return nil }
func (c *chanStream) Close() error           { return nil }

func serve(t *testing.T, b adapter.Backend) *checker {
	t.Helper()
	exec := &adapter.Executor{Backend: b}
	var url string
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	url = srv.URL + "/a2a"
	srv.Config.Handler = adapter.Mux("/a2a", adapter.Card("test", "test", url), adapter.NewRequestHandler(exec))
	return newChecker(url)
}

// TestKourierClientDrivesAdapter runs Kourier's agent client and a2a dialect over HTTP against the adapter: a text
// turn, and a tool approval that Kourier reads as structured, allowed and denied.
func TestKourierClientDrivesAdapter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := &scripted{}
	c := serve(t, b)

	if err := c.text(ctx); err != nil {
		t.Fatalf("text: %v", err)
	}

	r, in, err := c.pause(ctx, "", "run bash hostname")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if in.Tools[0].ID != "sevt_ask" || in.Tools[0].Args["command"] != "hostname" {
		t.Fatalf("interaction tools = %+v", in.Tools)
	}
	if len(r.calls) != 1 || r.calls[0].Name != "bash" {
		t.Fatalf("tool calls seen = %+v", r.calls)
	}
	done, err := c.decide(ctx, in, true, "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if done.final.Task.Status.State != a2a.TaskStateCompleted || len(done.results) != 1 {
		t.Fatalf("after approve: state %s results %+v", done.final.Task.Status.State, done.results)
	}
	if !strings.Contains(done.allText(), "cma-sandbox-abc") {
		t.Fatalf("text = %q", done.allText())
	}

	_, in, err = c.pause(ctx, "", "run bash again")
	if err != nil {
		t.Fatalf("second pause: %v", err)
	}
	denied, err := c.decide(ctx, in, false, "no")
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if denied.final.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("after deny: %s", denied.final.Task.Status.State)
	}
	last := b.sent[len(b.sent)-1].Confirmation
	if last == nil || last.Allow || last.DenyMessage != "no" {
		t.Fatalf("deny sent %+v", last)
	}
}
