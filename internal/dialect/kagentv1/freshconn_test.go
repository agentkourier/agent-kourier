package kagentv1

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// freshServer is a front door that serves the agent card and counts its connections as they open and close.
type freshServer struct {
	*httptest.Server
	mu     sync.Mutex
	opened int
	protos []int         // HTTP major version of each request
	closed chan struct{} // one send per connection that closes
}

// newFreshServer starts a front door. tls and h2 choose HTTPS and HTTP/2; a plain server speaks HTTP/1.1 only.
func newFreshServer(t *testing.T, useTLS, h2 bool) *freshServer {
	t.Helper()
	fs := &freshServer{closed: make(chan struct{}, 64)}
	fs.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.protos = append(fs.protos, r.ProtoMajor)
		fs.mu.Unlock()
		card := a2a.AgentCard{Name: "ask", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://kagent-controller.invalid:8083"+agentPath, a2a.TransportProtocolJSONRPC)}}
		card.Capabilities.Streaming = true
		_ = json.NewEncoder(w).Encode(card)
	}))
	fs.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			fs.mu.Lock()
			fs.opened++
			fs.mu.Unlock()
		case http.StateClosed:
			fs.closed <- struct{}{}
		}
	}
	switch {
	case useTLS:
		fs.EnableHTTP2 = h2
		fs.StartTLS()
	default:
		fs.Start()
	}
	t.Cleanup(fs.Close)
	return fs
}

func (fs *freshServer) connections() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.opened
}

// waitClosed waits until n more connections have closed.
func (fs *freshServer) waitClosed(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-fs.closed:
		case <-time.After(10 * time.Second):
			t.Fatal("a connection is still open 10 s after it should have closed")
		}
	}
}

// dialect returns a Dialect that trusts the server's certificate on both of its transports.
func (fs *freshServer) dialect(t *testing.T) *Dialect {
	t.Helper()
	d := New(0)
	if fs.TLS != nil {
		roots := fs.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		for _, rt := range []http.RoundTripper{d.base, d.fresh} {
			tr := rt.(*http.Transport)
			if tr.TLSClientConfig == nil {
				tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}
			tr.TLSClientConfig.RootCAs = roots // in place: replacing the config would drop the protocols New negotiates
		}
	}
	t.Cleanup(d.CloseIdleConnections)
	return d
}

func (fs *freshServer) connect(ctx context.Context, d *Dialect) error {
	backend := &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: fs.URL}}
	client, err := d.Connect(ctx, backend, agent, dialect.Credentials{Token: secret.New("test-token")})
	if err == nil {
		_ = client.Destroy()
	}
	return err
}

var freshCases = []struct {
	name      string
	tls, h2   bool
	wantMajor int
}{
	{"HTTP/1.1", false, false, 1},
	{"HTTP/1.1 over TLS", true, false, 1},
	{"HTTP/2 over TLS", true, true, 2},
}

// A fresh connect is the fault a probe exists to catch: a front door that takes no new connection (an expired
// certificate, a DNS record that is gone, a NetworkPolicy that blocks new flows) while a connection a turn
// holds is still served. The connect of a turn goes on using that connection; the fresh one fails.
func TestFreshConnectionFailsWhereAHeldConnectionStillWorks(t *testing.T) {
	for _, tc := range freshCases {
		t.Run(tc.name, func(t *testing.T) {
			fs, ctx := newFreshServer(t, tc.tls, tc.h2), t.Context()
			d := fs.dialect(t)
			if err := fs.connect(ctx, d); err != nil { // a turn's connect: opens the connection that stays pooled
				t.Fatal(err)
			}
			_ = fs.Listener.Close() // no new connection from here on; the one held stays served
			if err := fs.connect(ctx, d); err != nil {
				t.Fatalf("a turn's connect with the connection held: %v", err)
			}
			if err := fs.connect(dialect.WithFreshConnection(ctx), d); err == nil {
				t.Error("a fresh connect succeeded though the front door takes no new connection: it rode the held one")
			}
			if n := fs.connections(); n != 1 {
				t.Errorf("%d connections, want the one a turn opened", n)
			}
		})
	}
}

// Each fresh connect opens a connection of its own and leaves none open, while connects of turns share one.
func TestFreshConnectionOpensOneConnectionPerCheckAndKeepsNone(t *testing.T) {
	for _, tc := range freshCases {
		t.Run(tc.name, func(t *testing.T) {
			fs, ctx := newFreshServer(t, tc.tls, tc.h2), t.Context()
			d := fs.dialect(t)
			connect := func(fresh bool) {
				t.Helper()
				c := ctx
				if fresh {
					c = dialect.WithFreshConnection(ctx)
				}
				if err := fs.connect(c, d); err != nil {
					t.Fatal(err)
				}
			}
			connect(false)
			for i, want := range []int{2, 3} {
				connect(true)
				if n := fs.connections(); n != want {
					t.Errorf("after fresh connect %d: %d connections, want %d", i+1, n, want)
				}
			}
			connect(false)
			if n := fs.connections(); n != 3 {
				t.Errorf("after a turn's connect: %d connections, want 3: it opened one instead of reusing", n)
			}
			fs.waitClosed(t, 2) // the two fresh connections close by themselves, with the held one still open
			d.CloseIdleConnections()
			fs.waitClosed(t, 1) // and closing the idle ones leaves nothing behind
			fs.mu.Lock()
			defer fs.mu.Unlock()
			for _, major := range fs.protos {
				if major != tc.wantMajor {
					t.Fatalf("request protocols = %v, want all HTTP/%d: the case does not test what it names", fs.protos, tc.wantMajor)
				}
			}
		})
	}
}

// A Dialect not built by New has no fresh transport and still connects.
func TestFreshConnectionOnADialectWithOneTransport(t *testing.T) {
	fs := newFreshServer(t, false, false)
	d := &Dialect{base: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(d.CloseIdleConnections)
	if err := fs.connect(dialect.WithFreshConnection(t.Context()), d); err != nil {
		t.Errorf("connect: %v", err)
	}
}

// The fresh path keeps the protections of the shared one: a redirect is not followed, so the Binding's
// token goes to the front door and to nowhere else, and a request for another host is refused.
func TestFreshConnectionKeepsTheTokenAtTheFrontDoor(t *testing.T) {
	var elsewhere int
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		elsewhere++
		mu.Unlock()
	}))
	defer other.Close()
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer door.Close()
	d := New(0)
	defer d.CloseIdleConnections()
	doorURL, err := url.Parse(door.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := dialect.WithFreshConnection(t.Context())
	creds := dialect.Credentials{Token: secret.New("test-token")}
	_, err = d.ConnectEndpoint(ctx, door.URL+agentPath, creds)
	var hs *dialect.HTTPStatusError
	if !errors.As(err, &hs) || hs.Code != http.StatusTemporaryRedirect {
		t.Errorf("connect through a redirecting front door = %v, want an HTTPStatusError 307", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, other.URL, nil)
	resp, err := d.httpClient(creds, doorURL).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Error("a fresh request for a host that is not the front door was sent")
	}
	mu.Lock()
	defer mu.Unlock()
	if elsewhere != 0 {
		t.Errorf("the other host got %d requests, want none", elsewhere)
	}
}
