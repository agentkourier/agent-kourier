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
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
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
// told apart by sentinels from the dialect.
func TestCheckTellsAnUnusableCardApart(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.card)
			}))
			t.Cleanup(srv.Close)
			tg := Target{
				Binding:     "team/sre",
				Backend:     &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: srv.URL}},
				Agent:       v1alpha1.AgentRef{Name: "ask", Namespace: "kagent"},
				Credentials: token("t"),
			}
			if err := newPool(t).Check(t.Context(), tg); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
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

func TestCheckAfterCloseFailsWithoutAConnect(t *testing.T) {
	fa, p := newFakeAgent(t, liveStream(t)), NewPool(kagentv1.New(0), quick)
	p.Close()
	if err := p.Check(t.Context(), targetFor(fa, "team/sre", token("t"))); !errors.Is(err, ErrPoolClosed) || fa.count("card") != 0 {
		t.Errorf("err = %v, card fetches = %d; want ErrPoolClosed and none", err, fa.count("card"))
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
