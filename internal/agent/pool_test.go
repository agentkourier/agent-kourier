package agent

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func token(tok string) CredentialsFunc {
	return func(context.Context) (dialect.Credentials, error) { return dialect.Credentials{Token: tok}, nil }
}

func targetFor(fa *fakeAgent, binding string, creds CredentialsFunc) Target {
	return Target{
		Binding:     binding,
		Backend:     &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: fa.URL}},
		Agent:       v1alpha1.AgentRef{Name: "ask", Namespace: "kagent"},
		Credentials: creds,
	}
}

func newPool(t *testing.T) *Pool {
	p := NewPool(kagentv1.New(0), quick)
	t.Cleanup(p.Close)
	return p
}

func liveStream(t *testing.T) func(rpc, http.ResponseWriter) {
	return func(_ rpc, w http.ResponseWriter) { sse(t, w, "livestream/01-slow-stream.response.sse", -1) }
}

// auths lists, per request in order, the method and the Authorization header it carried.
func (fa *fakeAgent) auths() []string {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	var out []string
	for _, c := range fa.calls {
		out = append(out, c.Method+" "+c.Header.Get("Authorization"))
	}
	return out
}

// The Pool wires the dialect's ToolPart in, so an Agent from it cannot miss tool parts.
func TestPoolAgentsRecognizeTheDialectsToolParts(t *testing.T) {
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { sse(t, w, "approval/01-pause-approve.response.sse", -1) })
	h, got := collector()
	if err := newPool(t).Agent(targetFor(fa, "team/sre", token("t"))).Send(t.Context(), message("add"), h); err != nil {
		t.Fatal(err)
	}
	if calls := slices.DeleteFunc(*got, func(e Event) bool { _, ok := e.(ToolCallEvent); return !ok }); len(calls) != 1 {
		t.Errorf("%d tool call events, want 1", len(calls))
	}
}

func TestPoolKeepsOneClientPerBindingAndAgent(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	sre, dev := targetFor(fa, "team/sre", token("t")), targetFor(fa, "team/dev", token("t"))
	other := sre
	other.Agent.Name = "other"
	for i, tc := range []struct {
		target    Target
		wantCards int
	}{{sre, 1}, {sre, 1}, {dev, 2}, {other, 3}, {sre, 3}} {
		if err := p.Agent(tc.target).Send(t.Context(), message("hi"), nil); err != nil {
			t.Fatal(err)
		}
		if n := fa.count("card"); n != tc.wantCards {
			t.Errorf("after send %d: %d card fetches, want %d", i+1, n, tc.wantCards)
		}
	}
}

// Tokens rotate (WP-21e): the provider runs for every call, and a new token gets a new
// client, whose card fetch and requests all carry it.
func TestCredentialsAreResolvedPerCallAndRotationReconnects(t *testing.T) {
	var tok atomic.Value
	tok.Store("tok-1")
	var resolved atomic.Int32
	creds := func(context.Context) (dialect.Credentials, error) {
		resolved.Add(1)
		return dialect.Credentials{Token: tok.Load().(string)}, nil
	}
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	agent := p.Agent(targetFor(fa, "team/sre", creds))
	for _, next := range []string{"", "", "tok-2"} {
		if next != "" {
			tok.Store(next)
		}
		if err := agent.Send(t.Context(), message("hi"), nil); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"card Bearer tok-1", "SendStreamingMessage Bearer tok-1", "SendStreamingMessage Bearer tok-1", "card Bearer tok-2", "SendStreamingMessage Bearer tok-2"}
	if got := fa.auths(); !slices.Equal(got, want) || resolved.Load() != 3 {
		t.Errorf("requests = %q, credential lookups = %d; want %q and 3", got, resolved.Load(), want)
	}
}

// A reconnect resolves credentials again, so a token that expired mid-stream is replaced.
func TestReconnectUsesTheCurrentToken(t *testing.T) {
	var lookups atomic.Int32
	creds := func(context.Context) (dialect.Credentials, error) {
		return dialect.Credentials{Token: "tok-" + string(rune('0'+lookups.Add(1)))}, nil
	}
	fa, p := newFakeAgent(t, resumeAfterFirstTick(t, false)), newPool(t)
	if err := p.Agent(targetFor(fa, "team/sre", creds)).Send(t.Context(), message("go slowly"), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"card Bearer tok-1", "SendStreamingMessage Bearer tok-1", "card Bearer tok-2", "GetTask Bearer tok-2", "SubscribeToTask Bearer tok-2"}
	if got := fa.auths(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// A configured token that cannot be read fails the call. Nothing is sent, and the
// identity never falls back to X-User-Id.
func TestNoCredentialsFailTheCallAndSendNothing(t *testing.T) {
	boom := errors.New("secret not found")
	for _, tc := range []struct {
		name     string
		creds    CredentialsFunc
		wantBoom bool
	}{
		{"the provider fails", func(context.Context) (dialect.Credentials, error) { return dialect.Credentials{UserID: "u"}, boom }, true},
		{"no identity", func(context.Context) (dialect.Credentials, error) { return dialect.Credentials{}, nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
			agent := p.Agent(targetFor(fa, "team/sre", tc.creds))
			if err := agent.Send(t.Context(), message("hi"), nil); err == nil || errors.Is(err, boom) != tc.wantBoom {
				t.Errorf("Send err = %v", err)
			}
			if got := fa.auths(); len(got) != 0 {
				t.Errorf("requests = %q, want none", got)
			}
		})
	}
}

// Without streaming a2a-go sends a unary message, and the 30 s header timeout cuts long tasks.
func TestPoolRefusesAnAgentThatDoesNotStream(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	fa.noStreaming = true
	err := p.Agent(targetFor(fa, "team/sre", token("t"))).Send(t.Context(), message("hi"), nil)
	if !errors.Is(err, ErrStreamingUnsupported) || fa.count("SendStreamingMessage") != 0 {
		t.Errorf("err = %v, want ErrStreamingUnsupported and no send", err)
	}
}

// Warm is for the Slack acknowledgement path: it must not wait for the card fetch, and the
// first send must not fetch it again.
func TestWarmReturnsAtOnceAndSharesTheConnect(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	fa.cardDelay = 300 * time.Millisecond
	tg := targetFor(fa, "team/sre", token("t"))
	start := time.Now()
	p.Warm(tg)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Warm took %v, want it to return before the card arrives", d)
	}
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); err != nil {
		t.Fatal(err)
	}
	if n := fa.count("card"); n != 1 {
		t.Errorf("%d card fetches, want 1", n)
	}
}

func TestCloseStopsWarmUpsAndRefusesNewCalls(t *testing.T) {
	cfg := quick
	cfg.ConnectTimeout = 5 * time.Second // the bound if Close fails to cancel the warm-up
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), cfg)
	fa.cardDelay = time.Minute
	tg := targetFor(fa, "team/sre", token("t"))
	p.Warm(tg)
	for fa.count("card") == 0 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	p.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v: it waited for the warm-up's card fetch instead of cancelling it", d)
	}
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Send after Close = %v, want ErrPoolClosed", err)
	}
	p.Warm(tg) // must not start anything
	fa.Close()
	goleak.VerifyNone(t)
}
