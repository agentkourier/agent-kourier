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
	"github.com/agentkourier/agent-kourier/internal/secret"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func token(tok string) CredentialsFunc {
	return func(context.Context) (dialect.Credentials, error) {
		return dialect.Credentials{Token: secret.New(tok)}, nil
	}
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

// An Agent from the Pool delivers tool parts, whatever the dialect it was made with.
func TestPoolAgentsDeliverToolParts(t *testing.T) {
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
		return dialect.Credentials{Token: secret.New(tok.Load().(string))}, nil
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
		return dialect.Credentials{Token: secret.New("tok-" + string(rune('0'+lookups.Add(1))))}, nil
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
	fa.cardGate = make(chan struct{}) // the card is held back until Warm has returned
	tg := targetFor(fa, "team/sre", token("t"))
	returns(t, "Warm", func() { p.Warm(tg) })
	close(fa.cardGate)
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); err != nil {
		t.Fatal(err)
	}
	if n := fa.count("card"); n != 1 {
		t.Errorf("%d card fetches, want 1", n)
	}
}

// returns fails the test if f is still running after 10 s. The window only bounds a hang;
// nothing asserts how fast f is, so a slow machine cannot fail it.
func returns(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return: it is waiting for the card fetch", what)
	}
}

func TestCloseStopsWarmUpsAndRefusesNewCalls(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	fa.cardGate = make(chan struct{}) // the card never comes
	tg := targetFor(fa, "team/sre", token("t"))
	p.Warm(tg)
	for fa.count("card") == 0 {
		time.Sleep(time.Millisecond)
	}
	returns(t, "Close", p.Close) // cancels the warm-up's fetch
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Send after Close = %v, want ErrPoolClosed", err)
	}
	p.Warm(tg) // must not start anything
	fa.Close()
	goleak.VerifyNone(t)
}

// lastCard returns the headers of the most recent card fetch fa got.
func (fa *fakeAgent) lastCard() http.Header {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	for _, c := range slices.Backward(fa.calls) {
		if c.Method == "card" {
			return c.Header
		}
	}
	return nil
}

// Anything that changes what a request carries must get a new client: a token, a user ID,
// a static header (they hold secrets too), the backend URL. The last two rows are pairs
// that a lazy encoding of the headers maps to one digest.
func TestSealCoversEveryCredentialInput(t *testing.T) {
	hdr := func(kv ...string) dialect.Headers {
		h := dialect.Headers{}
		for i := 0; i < len(kv); i += 2 {
			h[kv[i]] = append(h[kv[i]], secret.New(kv[i+1]))
		}
		return h
	}
	for _, tc := range []struct {
		name          string
		before, after dialect.Credentials
		wantHeader    string // on the new client's card fetch
		wantValues    []string
	}{
		{"token", dialect.Credentials{Token: secret.New("a")}, dialect.Credentials{Token: secret.New("b")}, "Authorization", []string{"Bearer b"}},
		{"user ID", dialect.Credentials{UserID: "a"}, dialect.Credentials{UserID: "b"}, "X-User-Id", []string{"b"}},
		{"static header", dialect.Credentials{Token: secret.New("t"), Headers: hdr("X-Api-Key", "k1")}, dialect.Credentials{Token: secret.New("t"), Headers: hdr("X-Api-Key", "k2")}, "X-Api-Key", []string{"k2"}},
		{"one value split in two", dialect.Credentials{Token: secret.New("t"), Headers: hdr("X-Api-Key", "k1 k2")}, dialect.Credentials{Token: secret.New("t"), Headers: hdr("X-Api-Key", "k1", "X-Api-Key", "k2")}, "X-Api-Key", []string{"k1", "k2"}},
		{"a header hidden in a value", dialect.Credentials{Token: secret.New("t"), Headers: hdr("X-A", "1] X-B:[2")}, dialect.Credentials{Token: secret.New("t"), Headers: hdr("X-A", "1", "X-B", "2")}, "X-B", []string{"2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
			cur := tc.before
			agent := p.Agent(targetFor(fa, "team/sre", func(context.Context) (dialect.Credentials, error) { return cur, nil }))
			for range 2 {
				if err := agent.Send(t.Context(), message("hi"), nil); err != nil {
					t.Fatal(err)
				}
			}
			if n := fa.count("card"); n != 1 {
				t.Fatalf("%d card fetches for unchanged credentials, want 1", n)
			}
			cur = tc.after
			if err := agent.Send(t.Context(), message("hi"), nil); err != nil {
				t.Fatal(err)
			}
			if got := fa.lastCard().Values(tc.wantHeader); fa.count("card") != 2 || !slices.Equal(got, tc.wantValues) {
				t.Errorf("%d card fetches, %s = %q; want a second fetch with %q", fa.count("card"), tc.wantHeader, got, tc.wantValues)
			}
		})
	}
}

func TestBackendURLChangeGetsANewClient(t *testing.T) {
	first, second := newFakeAgent(t, liveStream(t)), newFakeAgent(t, liveStream(t))
	p, tg := newPool(t), targetFor(first, "team/sre", token("t"))
	for _, fa := range []*fakeAgent{first, second} {
		tg.Backend = &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: fa.URL}}
		if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if first.count("card") != 1 || second.count("card") != 1 || second.count("SendStreamingMessage") != 1 {
		t.Errorf("cards: %d and %d, sends to the new URL: %d; want 1, 1, 1", first.count("card"), second.count("card"), second.count("SendStreamingMessage"))
	}
}

// Defense in depth: a Binding with tokenSecretRef never reaches the agent as an X-User-Id,
// whatever the provider returns.
func TestTokenRequiredRefusesAnEmptyToken(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	tg := targetFor(fa, "team/sre", func(context.Context) (dialect.Credentials, error) {
		return dialect.Credentials{UserID: "agent-kourier"}, nil
	})
	tg.TokenRequired = true
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); !errors.Is(err, ErrTokenRequired) || len(fa.auths()) != 0 {
		t.Errorf("err = %v, requests = %q; want ErrTokenRequired and nothing sent", err, fa.auths())
	}
	tg.Credentials = token("tok")
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); err != nil {
		t.Errorf("with a token: %v", err)
	}
}

// conn takes the entry's lock and only then checks for Close, so a call that waited for a
// connect to finish cannot start one afterwards. Nothing else guards it.
func TestConnAfterCloseFailsWithoutAConnect(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	p.Close()
	if _, err := p.conn(t.Context(), targetFor(fa, "team/sre", token("t"))); !errors.Is(err, ErrPoolClosed) || fa.count("card") != 0 {
		t.Errorf("err = %v, card fetches = %d; want ErrPoolClosed and none", err, fa.count("card"))
	}
}

// Close waits for the connects in progress, so it cancels them, a caller's included.
func TestCloseCancelsAConnectACallerIsWaitingOn(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	fa.cardGate = make(chan struct{}) // the card never comes
	sent := make(chan error, 1)
	go func() { sent <- p.Agent(targetFor(fa, "team/sre", token("t"))).Send(t.Context(), message("hi"), nil) }()
	for fa.count("card") == 0 {
		time.Sleep(time.Millisecond)
	}
	returns(t, "Close", p.Close)
	returns(t, "Send", func() {
		if err := <-sent; !errors.Is(err, ErrNotSent) {
			t.Errorf("Send = %v, want ErrNotSent: the connect was cancelled", err)
		}
	})
}
