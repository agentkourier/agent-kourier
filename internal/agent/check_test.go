package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// countingTransport is a transport that counts its Destroy, so a test can tell that every client a
// probe connected was destroyed. The calls it does not override are unimplemented: a probe makes none.
type countingTransport struct {
	a2aclient.Transport
	destroyed *atomic.Int32
}

func (c countingTransport) Destroy() error { c.destroyed.Add(1); return nil }

// countingDialect hands out clients whose transport counts its Destroy. It connects through the real
// dialect, so the card fetch, its credentials and its errors are the production ones, and then rebuilds
// the client from the card it got on the counting transport.
type countingDialect struct {
	dialect.Dialect
	created, destroyed atomic.Int32
}

func (d *countingDialect) Connect(ctx context.Context, backend *v1alpha1.AgentBackend, ag v1alpha1.AgentRef, creds dialect.Credentials) (*a2aclient.Client, error) {
	inner, err := d.Dialect.Connect(ctx, backend, ag, creds)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inner.Destroy() }()
	d.created.Add(1)
	return a2aclient.NewFromCard(ctx, inner.Card(), a2aclient.WithDefaultsDisabled(),
		a2aclient.WithTransport(a2a.TransportProtocolJSONRPC, a2aclient.TransportFactoryFn(
			func(context.Context, *a2a.AgentCard, *a2a.AgentInterface) (a2aclient.Transport, error) {
				return countingTransport{destroyed: &d.destroyed}, nil
			})))
}

// newCountingPool is a Pool whose clients are counted; it is closed when the test ends.
func newCountingPool(t *testing.T, cfg Config) (*Pool, *countingDialect) {
	t.Helper()
	d := &countingDialect{Dialect: kagentv1.New(0)}
	p := NewPool(d, cfg)
	t.Cleanup(p.Close)
	return p, d
}

// setCardStatus makes the next card fetches fail with code, or succeed again with 0. It takes the fake
// agent's lock, which its handler takes before it reads the field.
func (fa *fakeAgent) setCardStatus(code int) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.cardStatus = code
}

// The probe reports the card fetch's own failure, untyped: a 403 stays a 403 and is not a rejected
// credential, which only the caller that knows what a 403 means may decide. A 2xx other than 200 never
// reaches the dialect's status error; a2a-go reports it itself.
func TestCheckReportsTheCardFetch(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			fa := newFakeAgent(t, liveStream(t))
			fa.cardStatus = code
			err := newPool(t).Check(t.Context(), targetFor(fa, "team/sre", token("t")))
			var hs *dialect.HTTPStatusError
			if !errors.As(err, &hs) || hs.Code != code {
				t.Fatalf("err = %v, want an HTTPStatusError with code %d", err, code)
			}
			if errors.Is(err, ErrCredentialsRejected) {
				t.Errorf("err = %v: the probe labelled it a rejected credential", err)
			}
		})
	}
	t.Run("OK", func(t *testing.T) {
		fa := newFakeAgent(t, liveStream(t))
		if err := newPool(t).Check(t.Context(), targetFor(fa, "team/sre", token("t"))); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("No Content", func(t *testing.T) {
		fa := newFakeAgent(t, liveStream(t))
		fa.cardStatus = http.StatusNoContent
		err := newPool(t).Check(t.Context(), targetFor(fa, "team/sre", token("t")))
		var notOK *agentcard.ErrStatusNotOK
		var hs *dialect.HTTPStatusError
		if !errors.As(err, &notOK) || notOK.StatusCode != http.StatusNoContent || errors.As(err, &hs) {
			t.Errorf("err = %v, want a2a-go's ErrStatusNotOK for 204 and no HTTPStatusError", err)
		}
	})
}

func TestCheckRefusesAnAgentThatDoesNotStream(t *testing.T) {
	fa := newFakeAgent(t, liveStream(t))
	fa.noStreaming = true
	p, d := newCountingPool(t, quick)
	if err := p.Check(t.Context(), targetFor(fa, "team/sre", token("t"))); !errors.Is(err, ErrStreamingUnsupported) {
		t.Errorf("err = %v, want ErrStreamingUnsupported", err)
	}
	if d.created.Load() != 1 || d.destroyed.Load() != 1 {
		t.Errorf("%d clients created and %d destroyed, want the one refused client destroyed", d.created.Load(), d.destroyed.Load())
	}
}

// Credentials that cannot be used fail before any request, with the sentinel the caller classifies by.
func TestCheckFailsOnCredentialsWithoutAConnect(t *testing.T) {
	for _, tc := range []struct {
		name          string
		creds         CredentialsFunc
		tokenRequired bool
		want          error
	}{
		{"token unreadable", func(context.Context) (dialect.Credentials, error) {
			return dialect.Credentials{}, fmt.Errorf("%w: secret not found", ErrTokenUnreadable)
		}, true, ErrTokenUnreadable},
		{"header unreadable", func(context.Context) (dialect.Credentials, error) {
			return dialect.Credentials{}, fmt.Errorf("%w: header %q", ErrHeaderUnreadable, "X-Api-Key")
		}, false, ErrHeaderUnreadable},
		{"token required but absent", func(context.Context) (dialect.Credentials, error) {
			return dialect.Credentials{UserID: "agent-kourier"}, nil
		}, true, ErrTokenRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, liveStream(t))
			tg := targetFor(fa, "team/sre", tc.creds)
			tg.TokenRequired = tc.tokenRequired
			if err := newPool(t).Check(t.Context(), tg); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if got := fa.auths(); len(got) != 0 {
				t.Errorf("requests = %q, want none", got)
			}
		})
	}
}

// A required extension Agent Kourier does not speak, and a card with no JSON-RPC interface, are
// told apart by sentinels from the dialect, whichever of the two dialects connects.
func TestCheckTellsAnUnusableCardApart(t *testing.T) {
	for _, dl := range []struct {
		name string
		d    dialect.Dialect
		kind v1alpha1.Dialect
	}{{"kagent-v1", kagentv1.New(0), v1alpha1.DialectKagentV1}, {"a2a", generic.New(0), v1alpha1.DialectA2A}} {
		for _, tc := range []struct {
			name string
			card a2a.AgentCard
			want error
		}{
			{"required extension", func() a2a.AgentCard {
				c := a2a.AgentCard{Name: "ask", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://x", a2a.TransportProtocolJSONRPC)}}
				c.Capabilities.Streaming = true
				c.Capabilities.Extensions = []a2a.AgentExtension{{URI: "https://example.test/extensions/other/v1", Required: true}}
				return c
			}(), dialect.ErrUnsupportedExtension},
			{"no JSON-RPC interface", func() a2a.AgentCard {
				c := a2a.AgentCard{Name: "ask", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://x", a2a.TransportProtocolGRPC)}}
				c.Capabilities.Streaming = true
				return c
			}(), dialect.ErrNoJSONRPCInterface},
		} {
			t.Run(dl.name+"/"+tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(tc.card)
				}))
				t.Cleanup(srv.Close)
				p := NewPool(dl.d, quick)
				t.Cleanup(p.Close)
				tg := Target{
					Binding:     "team/sre",
					Backend:     &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: dl.kind, URL: srv.URL}},
					Agent:       v1alpha1.AgentRef{Name: "ask", Namespace: "kagent"},
					Credentials: token("t"),
				}
				if err := p.Check(t.Context(), tg); !errors.Is(err, tc.want) {
					t.Errorf("err = %v, want %v", err, tc.want)
				}
			})
		}
	}
}

// A probe sends the agent no message: the one request it makes is the card fetch.
func TestCheckSendsOnlyTheCardFetch(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	if err := p.Check(t.Context(), targetFor(fa, "team/sre", token("t"))); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.calls) != 1 || fa.calls[0].Method != "card" {
		t.Errorf("the agent got %d requests, want the one card fetch", len(fa.calls))
	}
}

// A probe that waits on a slow backend holds nothing a turn needs: a turn on the cached client goes on.
func TestASlowProbeDoesNotStallATurn(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	tg := targetFor(fa, "team/sre", token("t"))
	ag := p.Agent(tg)
	if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	fa.mu.Lock()
	fa.cardGate = gate // the probe's card never comes until the gate closes
	fa.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- p.Check(t.Context(), tg) }()
	for fa.count("card") < 2 {
		time.Sleep(time.Millisecond)
	}
	returns(t, "Send", func() {
		if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
			t.Errorf("Send while a probe waits: %v", err)
		}
	})
	close(gate)
	if err := <-done; err != nil {
		t.Errorf("Check: %v", err)
	}
}

// The reason Check connects afresh: after a turn the pool holds a client, and a probe through it would
// keep passing while the backend was down. The probe must fail, and the client must stay the pool's.
func TestCheckSeesABackendThatFailedAfterATurnAndLeavesTheCachedClient(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	tg := targetFor(fa, "team/sre", token("t"))
	ag := p.Agent(tg)
	if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(t.Context(), tg); err != nil {
		t.Fatalf("Check while the backend is up: %v", err)
	}
	if fa.count("card") != 2 {
		t.Fatalf("%d card fetches after a turn and a probe, want 2: the probe must not use the cached client", fa.count("card"))
	}

	fa.setCardStatus(http.StatusServiceUnavailable)
	err := p.Check(t.Context(), tg)
	var hs *dialect.HTTPStatusError
	if !errors.As(err, &hs) || hs.Code != http.StatusServiceUnavailable {
		t.Fatalf("Check with the backend down = %v, want an HTTPStatusError 503", err)
	}

	// The turn's client is untouched: the next turn sends without another card fetch, though the card is down.
	if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
		t.Errorf("Send after the failed probe: %v", err)
	}
	if fa.count("card") != 3 || fa.count("SendStreamingMessage") != 2 {
		t.Errorf("%d card fetches and %d sends, want 3 and 2: the probe replaced or dropped the cached client", fa.count("card"), fa.count("SendStreamingMessage"))
	}
}

// A probe neither reads nor writes the cache: it does not seed it, and it does not reseal it for the
// credentials it was given.
func TestCheckNeitherSeedsNorResealsTheCache(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	tg := targetFor(fa, "team/sre", token("t"))
	if err := p.Check(t.Context(), tg); err != nil {
		t.Fatal(err)
	}
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); err != nil {
		t.Fatal(err)
	}
	if fa.count("card") != 2 {
		t.Errorf("%d card fetches after a probe and a first turn, want 2: the probe seeded the cache", fa.count("card"))
	}
	other := tg
	other.Credentials = token("rotated")
	if err := p.Check(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if err := p.Agent(tg).Send(t.Context(), message("hi"), nil); err != nil {
		t.Fatal(err)
	}
	if fa.count("card") != 3 {
		t.Errorf("%d card fetches, want 3: the probe with other credentials evicted the cached client", fa.count("card"))
	}
}

// Every probe fetches the card, whatever the last one found, and destroys the client it made.
func TestCheckFetchesTheCardEveryCallAndDestroysItsClient(t *testing.T) {
	fa := newFakeAgent(t, liveStream(t))
	p, d := newCountingPool(t, quick)
	tg := targetFor(fa, "team/sre", token("t"))
	for i := 1; i <= 3; i++ {
		if err := p.Check(t.Context(), tg); err != nil {
			t.Fatal(err)
		}
		if n := fa.count("card"); n != i {
			t.Errorf("after %d probes: %d card fetches", i, n)
		}
	}
	if d.created.Load() != 3 || d.destroyed.Load() != 3 {
		t.Errorf("%d clients created and %d destroyed, want 3 and 3", d.created.Load(), d.destroyed.Load())
	}
}

// After Close the probe neither reads the credentials nor connects.
func TestCheckAfterCloseFailsWithoutAConnect(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	p.Close()
	var lookups atomic.Int32
	tg := targetFor(fa, "team/sre", func(context.Context) (dialect.Credentials, error) {
		lookups.Add(1)
		return dialect.Credentials{}, nil
	})
	if err := p.Check(t.Context(), tg); !errors.Is(err, ErrPoolClosed) || fa.count("card") != 0 || lookups.Load() != 0 {
		t.Errorf("err = %v, card fetches = %d, credential lookups = %d; want ErrPoolClosed and none", err, fa.count("card"), lookups.Load())
	}
}

// Close ends a probe that waits for a card, and the probe says why.
func TestCloseEndsAProbeWaitingOnTheCard(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	fa.cardGate = make(chan struct{}) // the card never comes
	done := make(chan error, 1)
	go func() { done <- p.Check(t.Context(), targetFor(fa, "team/sre", token("t"))) }()
	for fa.count("card") == 0 {
		time.Sleep(time.Millisecond)
	}
	p.Close()
	returns(t, "Check", func() {
		if err := <-done; !errors.Is(err, ErrPoolClosed) {
			t.Errorf("Check = %v, want ErrPoolClosed", err)
		}
	})
}

// A backend that never answers the card ends the probe at ConnectTimeout, not the caller's deadline.
func TestCheckIsBoundedByTheConnectTimeout(t *testing.T) {
	fa := newFakeAgent(t, liveStream(t))
	fa.cardGate = make(chan struct{})
	p := newPool(t)
	p.cfg.ConnectTimeout = 50 * time.Millisecond
	returns(t, "Check", func() {
		if err := p.Check(t.Context(), targetFor(fa, "team/sre", token("t"))); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Check = %v, want context.DeadlineExceeded", err)
		}
	})
}

// The reason Check asks for a fresh connection: a turn holds a connection to the front door open, and a card fetch
// that rode it would pass while the front door took no new connection (an expired certificate, a DNS record that is
// gone, a NetworkPolicy that blocks new flows). Here the listener stops accepting and the turn's connection stays
// open: the turn goes on and the probe fails. The dialects' tests count connections, over TLS and HTTP/2 as well.
func TestCheckFailsWhereATurnsHeldConnectionStillWorks(t *testing.T) {
	for _, dl := range []struct {
		name string
		d    dialect.Dialect
	}{{"kagent-v1", kagentv1.New(0)}, {"a2a", generic.New(0)}} {
		t.Run(dl.name, func(t *testing.T) {
			fa := newFakeAgent(t, liveStream(t))
			p := NewPool(dl.d, quick)
			t.Cleanup(p.Close)
			tg := targetFor(fa, "team/sre", token("t"))
			ag := p.Agent(tg)
			if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
				t.Fatal(err)
			}
			if err := p.Check(t.Context(), tg); err != nil {
				t.Fatalf("Check while the front door is up: %v", err)
			}
			_ = fa.Listener.Close() // no new connection from here on; the turn's stays served
			if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
				t.Fatalf("Send over the held connection: %v", err)
			}
			if err := p.Check(t.Context(), tg); err == nil || errors.Is(err, ErrPoolClosed) {
				t.Errorf("Check = %v, want a connection error: the probe rode the connection a turn holds", err)
			}
			if err := ag.Send(t.Context(), message("hi"), nil); err != nil {
				t.Errorf("Send after the failed probe: %v", err)
			}
		})
	}
}

// stuckDialect is a dialect whose Connect says it has begun and then ignores its context until released, as a
// card fetch stuck in a read does. Cancelling the context does not end it, so only a Close that waits is slower than it.
type stuckDialect struct {
	dialect.Dialect
	entered chan struct{}
	release chan struct{}
}

func (d stuckDialect) Connect(ctx context.Context, _ *v1alpha1.AgentBackend, _ v1alpha1.AgentRef, _ dialect.Credentials) (*a2aclient.Client, error) {
	d.entered <- struct{}{}
	<-d.release
	return nil, ctx.Err()
}

// closeWaitsForCheck runs Check on p while hold has it in flight, calls Close, and shows that Close does not return
// until release lets the Check go, and that the Check says ErrPoolClosed.
func closeWaitsForCheck(t *testing.T, p *Pool, tg Target, entered <-chan struct{}, release func()) {
	t.Helper()
	checked := make(chan error, 1)
	go func() { checked <- p.Check(t.Context(), tg) }()
	returns(t, "Check to start", func() { <-entered })
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	<-p.bg.Done() // Close has cancelled what the pool's work runs under
	select {
	case <-closed:
		t.Error("Close returned while a Check was in flight")
	case <-time.After(100 * time.Millisecond): // a Close that does not wait returns within microseconds; the window can hide a bug, never fail a correct Close
	}
	release()
	returns(t, "Close", func() { <-closed })
	returns(t, "Check", func() {
		if err := <-checked; !errors.Is(err, ErrPoolClosed) {
			t.Errorf("Check = %v, want ErrPoolClosed", err)
		}
	})
}

// Close waits for a probe, so no goroutine of it outlives Close, and no card request that carries the token starts after
// Close has returned.
func TestCloseWaitsForAProbeInFlight(t *testing.T) {
	t.Run("on a slow card fetch", func(t *testing.T) {
		d := stuckDialect{Dialect: kagentv1.New(0), entered: make(chan struct{}, 1), release: make(chan struct{})}
		p := NewPool(d, quick)
		closeWaitsForCheck(t, p, targetFor(newFakeAgent(t, liveStream(t)), "team/sre", token("t")), d.entered, func() { close(d.release) })
	})
	t.Run("on a slow credential source", func(t *testing.T) {
		fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		slow := func(context.Context) (dialect.Credentials, error) { // ignores its context, which a source must not
			entered <- struct{}{}
			<-release
			return dialect.Credentials{Token: secret.New("t")}, nil
		}
		closeWaitsForCheck(t, p, targetFor(fa, "team/sre", slow), entered, func() { close(release) })
		if n := fa.count("card"); n != 0 {
			t.Errorf("%d card fetches, want none: a probe sent the token after Close", n)
		}
	})
}

// A credential source that honors its context is cut short by Close, so Close is bounded by the same context as the connect.
func TestCloseEndsAProbeWaitingOnTheCredentials(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	entered := make(chan struct{}, 1)
	waits := func(ctx context.Context) (dialect.Credentials, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return dialect.Credentials{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- p.Check(t.Context(), targetFor(fa, "team/sre", waits)) }()
	returns(t, "Check to start", func() { <-entered })
	returns(t, "Close", p.Close)
	returns(t, "Check", func() {
		if err := <-done; !errors.Is(err, ErrPoolClosed) {
			t.Errorf("Check = %v, want ErrPoolClosed", err)
		}
	})
	if n := fa.count("card"); n != 0 {
		t.Errorf("%d card fetches, want none", n)
	}
}

// The ConnectTimeout bounds the credentials read too, and the error is the timeout's.
func TestCheckIsBoundedOnTheCredentialsByTheConnectTimeout(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), newPool(t)
	p.cfg.ConnectTimeout = 50 * time.Millisecond
	waits := func(ctx context.Context) (dialect.Credentials, error) {
		<-ctx.Done()
		return dialect.Credentials{}, ctx.Err()
	}
	returns(t, "Check", func() {
		if err := p.Check(t.Context(), targetFor(fa, "team/sre", waits)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Check = %v, want context.DeadlineExceeded", err)
		}
	})
}
