package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/secret"
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

// testToken is the adapter's one accepted token in these tests: an obviously fake value.
const testToken = "fake-livecheck-token"

// serve runs the adapter over b, as main wires it: a SQLite store and the caller check, with testToken accepted.
// It returns the adapter's JSON-RPC URL.
func serve(t *testing.T, b adapter.Backend) string {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "cma.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tokenFile := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	tokens, err := auth.LoadTokens(tokenFile, log)
	if err != nil {
		t.Fatal(err)
	}
	exec := &adapter.Executor{Backend: b, Store: st}
	rpc := a2asrv.NewJSONRPCHandler(adapter.NewRequestHandler(exec, a2asrv.WithCallInterceptors(auth.Interceptor{})))
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	url := srv.URL + "/a2a"
	check := &auth.Check{Verifier: auth.TokenVerifier{Header: "Authorization", Tokens: tokens}, Bearer: true, Log: log}
	srv.Config.Handler = adapter.Mux("/a2a", adapter.Card("test", "test", url, "test"), rpc, check)
	return url
}

// The caller check takes the token as Agent Kourier's a2a dialect sends a Binding's, and refuses the dialect without it.
func TestKourierClientPassesTheCallerCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := serve(t, &scripted{})
	for name, c := range map[string]*checker{"no token": newChecker(url, ""), "wrong token": newChecker(url, "fake-wrong-token")} {
		if err := c.text(ctx); err == nil {
			t.Errorf("%s: the adapter ran the turn", name)
		}
	}
	if err := newChecker(url, testToken).text(ctx); err != nil {
		t.Fatalf("with the token: %v", err)
	}
}

// Agent Kourier's a2a dialect fetches the card, the first request of a connect, with the Binding's token: the card is
// behind the caller check, so a connect without the token fails there with 401, and one with it gets the card.
func TestKourierFetchesTheCardWithTheToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := serve(t, &scripted{})
	d := generic.New(30 * time.Second)
	backend := &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: url}}
	ref := v1alpha1.AgentRef{Name: "cma"}
	for name, creds := range map[string]dialect.Credentials{
		"no token":    {UserID: "livecheck"},
		"wrong token": {Token: secret.New("fake-wrong-token")},
	} {
		_, err := d.Connect(ctx, backend, ref, creds)
		var status *dialect.HTTPStatusError
		if !errors.As(err, &status) || status.Code != http.StatusUnauthorized {
			t.Errorf("%s: connect = %v, want the card refused with 401", name, err)
		}
	}
	client, err := d.Connect(ctx, backend, ref, dialect.Credentials{Token: secret.New(testToken)})
	if err != nil {
		t.Fatalf("with the token: %v", err)
	}
	if got := client.Card().Name; got != "test" {
		t.Fatalf("card name = %q, want the adapter's", got)
	}
	_ = client.Destroy()
}

// TestKourierClientDrivesAdapter runs Kourier's agent client and a2a dialect over HTTP against the adapter: a text
// turn, and a tool approval that Kourier reads as structured, allowed and denied.
func TestKourierClientDrivesAdapter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := &scripted{}
	c := newChecker(serve(t, b), testToken)

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
