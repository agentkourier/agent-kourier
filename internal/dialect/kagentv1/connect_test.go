package kagentv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/log"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

const agentPath = "/agents/kagent/spike-ask"

// frontDoor stands in for the verifying front door: it serves the agent card, which (like
// kagent's) advertises a fenced in-cluster URL, and replays WP-02's ask-user fixtures.
type frontDoor struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newFrontDoor(t *testing.T, hitlListed bool, card, send http.HandlerFunc) *frontDoor {
	t.Helper()
	fd := &frontDoor{}
	fd.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fd.mu.Lock()
		fd.reqs = append(fd.reqs, r)
		fd.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == agentPath+"/.well-known/agent-card.json" && card != nil:
			card(w, r)
		case r.Method == http.MethodGet && r.URL.Path == agentPath+"/.well-known/agent-card.json":
			c := a2a.AgentCard{Name: "spike-ask", Version: "1", SupportedInterfaces: []*a2a.AgentInterface{
				a2a.NewAgentInterface("http://kagent-controller.invalid:8083"+agentPath, a2a.TransportProtocolJSONRPC),
				a2a.NewAgentInterface("http://kagent-controller.invalid:8083/rest", a2a.TransportProtocolHTTPJSON),
			}}
			c.Capabilities.Streaming = true // else the client falls back to a unary send
			if hitlListed {
				c.Capabilities.Extensions = []a2a.AgentExtension{{URI: HITLExtension}}
			}
			_ = json.NewEncoder(w).Encode(c)
		case r.Method == http.MethodPost && r.URL.Path == agentPath && send != nil:
			send(w, r)
		case r.Method == http.MethodPost && r.URL.Path == agentPath:
			name := "01-ask-without-extension-header.response.sse" // kagent sends the payload only if asked
			if r.Header.Get("A2A-Extensions") == HITLExtension {
				name = "02-ask.response.sse"
			}
			body, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "kagent", "askuser", name))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fd.Close)
	return fd
}

// seen returns the headers of the requests so far that used method.
func (fd *frontDoor) seen(method string) []http.Header {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	var hs []http.Header
	for _, r := range fd.reqs {
		if r.Method == method {
			hs = append(hs, r.Header)
		}
	}
	return hs
}

func (fd *frontDoor) backend() *v1alpha1.AgentBackend {
	return &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: fd.URL}}
}

// ask connects and sends one message, returning the final task the stream carried.
func ask(ctx context.Context, fd *frontDoor, creds dialect.Credentials) (*a2a.Task, error) {
	client, err := New(0).Connect(ctx, fd.backend(), agent, creds)
	if err != nil {
		return nil, err
	}
	var last a2a.Event
	for ev, err := range client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Which database?"))}) {
		if err != nil {
			return nil, err
		}
		last = ev
	}
	task, ok := last.(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("stream ended with %T, want a task", last)
	}
	return task, nil
}

func TestConnectOverridesCardURL(t *testing.T) {
	fd := newFrontDoor(t, true, nil, nil)
	if _, err := ask(t.Context(), fd, dialect.Credentials{Token: secret.New(canary)}); err != nil {
		t.Fatalf("ask: %v (the card's :8083 URL, or its REST interface, was used)", err)
	}
	if n := len(fd.seen(http.MethodPost)); n != 1 {
		t.Errorf("front door got %d sends, want 1", n)
	}
}

func TestConnectHITLHeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed bool
	}{{"card lists the extension", true}, {"card lists no extension", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fd := newFrontDoor(t, tc.listed, nil, nil)
			task, err := ask(t.Context(), fd, dialect.Credentials{Token: secret.New(canary)})
			if err != nil {
				t.Fatal(err)
			}
			sent := fd.seen(http.MethodPost)[0].Get("A2A-Extensions") == HITLExtension
			_, payload := task.Status.Message.Metadata[HITLExtension]
			if sent != tc.listed || payload != tc.listed {
				t.Errorf("header sent = %v, payload = %v; want both %v", sent, payload, tc.listed)
			}
		})
	}
}

func TestConnectIdentityOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name               string
		creds              dialect.Credentials
		wantAuth, wantUser string
	}{
		{"oidc", dialect.Credentials{Token: secret.New(canary), UserID: "agent-kourier-sre"}, "Bearer " + canary, ""},
		{"insecure", dialect.Credentials{UserID: "agent-kourier-sre"}, "", "agent-kourier-sre"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := newFrontDoor(t, true, nil, nil)
			if _, err := ask(t.Context(), fd, tc.creds); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} { // the card fetch and the send
				for _, h := range fd.seen(method) {
					if h.Get("Authorization") != tc.wantAuth || h.Get("X-User-Id") != tc.wantUser {
						t.Errorf("%s: Authorization=%q X-User-Id=%q, want %q and %q", method, h.Get("Authorization"), h.Get("X-User-Id"), tc.wantAuth, tc.wantUser)
					}
				}
			}
		})
	}
}

// A response that is not 2xx comes back as a typed error, so a caller can tell a request the
// front door refused from a connection that dropped. The body never reaches the error.
func TestNon2xxResponseIsAnHTTPStatusError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusBadGateway, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			fd := newFrontDoor(t, true, nil, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, canary, code) })
			_, err := ask(t.Context(), fd, dialect.Credentials{Token: secret.New(canary)})
			var hs *dialect.HTTPStatusError
			if !errors.As(err, &hs) || hs.Code != code {
				t.Fatalf("err = %v, want an *HTTPStatusError with code %d", err, code)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the response body leaked into the error: %v", err)
			}
		})
	}
}

// A 429 or 503 may say when to come back, in a header. The error carries that and nothing
// from the body.
func TestHTTPStatusErrorCarriesRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		min, max     time.Duration
	}{
		{"seconds", "7", 7 * time.Second, 7 * time.Second},
		{"a date", time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), 60 * time.Second, 90 * time.Second},
		{"a date in the past", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0, 0},
		{"garbage", "soon", 0, 0},
		{"negative", "-5", 0, 0},
		{"a plus sign: delay-seconds is digits only", "+5", 0, 0},
		{"a day", "86400", 24 * time.Hour, 24 * time.Hour},
		{"a second over a day: capped", "86401", 24 * time.Hour, 24 * time.Hour},
		{"twenty digits: capped, not zero", "99999999999999999999", 24 * time.Hour, 24 * time.Hour},
		{"absent", "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := newFrontDoor(t, true, nil, func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				http.Error(w, canary, http.StatusTooManyRequests)
			})
			_, err := ask(t.Context(), fd, dialect.Credentials{Token: secret.New(canary)})
			var hs *dialect.HTTPStatusError
			if !errors.As(err, &hs) || hs.RetryAfter < tc.min || hs.RetryAfter > tc.max || strings.Contains(err.Error(), canary) {
				t.Errorf("err = %v (%+v), want RetryAfter of %v to %v and no body", err, hs, tc.min, tc.max)
			}
		})
	}
}

func TestConnectFailuresLeakNothing(t *testing.T) {
	other, stolen := otherHost(t) // the redirect target, which must never be called
	for _, tc := range []struct {
		name  string
		creds dialect.Credentials
		card  http.HandlerFunc
		send  http.HandlerFunc
	}{
		{"reserved static header", dialect.Credentials{Token: secret.New(canary), Headers: hdr("Authorization", canary)}, nil, nil},
		{"token with a line break", dialect.Credentials{Token: secret.New(canary + "\n")}, nil, nil},
		{"card rejects the token", dialect.Credentials{Token: secret.New(canary)}, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, canary, http.StatusUnauthorized) }, nil},
		{"card redirects elsewhere", dialect.Credentials{Token: secret.New(canary)}, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }, nil},
		{"send redirects elsewhere", dialect.Credentials{Token: secret.New(canary)}, nil, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect) // a 307 would replay the POST and its headers
		}},
		{"card is not JSON", dialect.Credentials{Token: secret.New(canary)}, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(canary)) }, nil},
		{"card lists only a REST interface", dialect.Credentials{Token: secret.New(canary)}, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"supportedInterfaces":[{"url":"http://x","protocolBinding":"HTTP+JSON","protocolVersion":"1.0"}]}`))
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := log.AttachLogger(t.Context(), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			fd := newFrontDoor(t, true, tc.card, tc.send)
			_, err := ask(ctx, fd, tc.creds)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), canary) || strings.Contains(logs.String(), canary) {
				t.Errorf("secret leaked.\nerror: %v\nlog: %s", err, logs.String())
			}
			if tc.send == nil && len(fd.seen(http.MethodPost)) != 0 {
				t.Error("a send went out after the failure")
			}
		})
	}
	if n := stolen.Load(); n != 0 {
		t.Errorf("a redirect was followed: the target got %d requests", n)
	}
}

func TestResponseTimeoutBoundsHeadersNotStreams(t *testing.T) {
	const limit = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall" {
			select { // never answers within the limit
			case <-time.After(5 * limit):
			case <-r.Context().Done():
			}
			return
		}
		w.(http.Flusher).Flush() // headers now, body for three times the limit
		for range 6 {
			_, _ = fmt.Fprint(w, "tick\n")
			w.(http.Flusher).Flush()
			time.Sleep(limit / 2)
		}
	}))
	t.Cleanup(srv.Close)
	hc := New(limit).httpClient(dialect.Credentials{UserID: "u"}, &url.URL{Scheme: "http", Host: srv.Listener.Addr().String()})
	get := func(path string) (string, error) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, http.NoBody)
		resp, err := hc.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		var b bytes.Buffer
		_, err = b.ReadFrom(resp.Body)
		return b.String(), err
	}
	if _, err := get("/stall"); err == nil {
		t.Error("a server that never sends headers was not cut off")
	}
	if body, err := get("/stream"); err != nil || strings.Count(body, "tick") != 6 {
		t.Errorf("stream = %q, %v; want all six ticks", body, err)
	}
}

// otherHost is a server that must never be called; hits counts the calls it gets.
func otherHost(t *testing.T) (srv *httptest.Server, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestConnectPointsEveryJSONRPCInterfaceAtTheFrontDoor(t *testing.T) {
	foreign, hits := otherHost(t)
	card := func(w http.ResponseWriter, _ *http.Request) { // the foreign interface comes first, so it would be chosen
		c := a2a.AgentCard{Name: "x", Version: "1", SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(foreign.URL+agentPath, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface("http://kagent-controller.invalid:8083"+agentPath, a2a.TransportProtocolJSONRPC),
		}}
		c.Capabilities.Streaming = true
		_ = json.NewEncoder(w).Encode(c)
	}
	if _, err := ask(t.Context(), newFrontDoor(t, false, card, nil), dialect.Credentials{Token: secret.New(canary)}); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the card's foreign interface got %d requests", n)
	}
}

func TestCredentialsGoOnlyToTheFrontDoor(t *testing.T) {
	foreign, hits := otherHost(t) // listens on plain http; every request below targets it
	for _, tc := range []struct {
		name      string
		frontDoor url.URL
	}{
		{"another host", url.URL{Scheme: "http", Host: "front-door.example:4180"}},
		{"same host, https configured, http request: no downgrade", url.URL{Scheme: "https", Host: foreign.Listener.Addr().String()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc := New(0).httpClient(dialect.Credentials{Token: secret.New(canary)}, &tc.frontDoor)
			body := &spyBody{Reader: strings.NewReader("x")}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, foreign.URL, body)
			resp, err := hc.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("the request was sent")
			}
			if strings.Contains(err.Error(), canary) || hits.Load() != 0 || !body.closed {
				t.Errorf("err = %v, hits = %d, body closed = %v", err, hits.Load(), body.closed)
			}
		})
	}
}

// spyBody records whether it was closed.
type spyBody struct {
	io.Reader
	closed bool
}

func (b *spyBody) Close() error { b.closed = true; return nil }

// The bearer token must not follow HTTP_PROXY. net/http caches that variable per process,
// so the test pins the transport instead of the environment.
func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	if tr, ok := New(0).base.(*http.Transport); !ok || tr.Proxy != nil {
		t.Errorf("transport = %T with Proxy set, want a Transport that dials directly", New(0).base)
	}
}

func TestConnectNeedsNew(t *testing.T) {
	be := &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{URL: "http://fd:4180"}}
	if _, err := (&Dialect{}).Connect(t.Context(), be, agent, dialect.Credentials{UserID: "u"}); err == nil {
		t.Error("a zero Dialect connected")
	}
}
