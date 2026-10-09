package fakeslack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// leakCheck fails the test if it leaves goroutines behind. It registers its
// check first so that it runs after every other cleanup.
func leakCheck(t *testing.T) {
	t.Helper()
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
}

// newSocket starts a fake Slack and a slack-go Socket Mode client running
// against it, the way Agent Kourier connects. The client is stopped on cleanup.
func newSocket(t *testing.T, opts ...fakeslack.Option) (*fakeslack.Server, *socketmode.Client) {
	t.Helper()
	leakCheck(t)
	fake := fakeslack.New(opts...)
	t.Cleanup(fake.Close)
	return fake, runClient(t, fake.URL())
}

// runClient runs a slack-go Socket Mode client against the fake at baseURL until
// the test ends.
func runClient(t *testing.T, baseURL string) *socketmode.Client {
	t.Helper()
	api := slack.New(testToken, slack.OptionAPIURL(baseURL+"/"), slack.OptionAppLevelToken(testAppToken))
	smc := socketmode.New(api)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = smc.RunContext(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return smc
}

// waitEvent returns the next client event of type typ, skipping the client's
// own connection events. The deadline is only how long an event that never comes is waited for, and is long: a loaded
// machine can starve the goroutine that sends it.
func waitEvent(t *testing.T, smc *socketmode.Client, typ socketmode.EventType) socketmode.Event {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev := <-smc.Events:
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %q event within 30s", typ)
		}
	}
}

// dial opens a raw websocket to the fake, as a Socket Mode client would.
func dial(t *testing.T, fake *fakeslack.Server) *websocket.Conn {
	t.Helper()
	api := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"), slack.OptionAppLevelToken(testAppToken))
	_, wsURL, err := api.StartSocketModeContext(t.Context())
	if err != nil {
		t.Fatalf("apps.connections.open: %v", err)
	}
	ws, resp, err := websocket.DefaultDialer.DialContext(t.Context(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// eventually polls cond until it holds. The deadline is only how long a condition that will never hold is waited
// for before the test fails, and is long: a loaded machine can starve the goroutine that makes it hold.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestSocketModeSendsHello(t *testing.T) {
	fake, smc := newSocket(t, fakeslack.WithBot("UBOT2", "BBOT2", "AAPP2"))
	hello := waitEvent(t, smc, socketmode.EventTypeHello)
	if got := hello.Request.ConnectionInfo.AppID; got != "AAPP2" {
		t.Fatalf("hello app_id = %q; want the configured one", got)
	}
	if n := fake.Connections(); n != 1 {
		t.Fatalf("Connections() = %d after hello; want 1", n)
	}
}

// TestNewHandlerServesSocketMode checks the documented way to run the fake
// behind another listener: NewHandler mounted at /, reached directly.
func TestNewHandlerServesSocketMode(t *testing.T) {
	leakCheck(t)
	h := fakeslack.NewHandler()
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	smc := runClient(t, srv.URL)
	waitEvent(t, smc, socketmode.EventTypeHello)
	if n := h.Connections(); n != 1 {
		t.Fatalf("Connections() = %d after hello; want 1", n)
	}
}

func TestClosedServerRefusesSocketClients(t *testing.T) {
	leakCheck(t)
	h := fakeslack.NewHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	h.Close()
	_, resp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/link", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial after Close = %v, %v; want a 503 refusal", resp, err)
	}
}

func TestClientDisconnectIsHandled(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	eventually(t, "the connection", func() bool { return fake.Connections() == 1 })

	_ = ws.Close()
	eventually(t, "the disconnect", func() bool { return fake.Connections() == 0 })
}

func TestCloseDropsConnections(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	fake.Close()
	if n := fake.Connections(); n != 0 {
		t.Fatalf("Connections() = %d after Close; want 0", n)
	}
}

func TestPingsCanBeTurnedOff(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New(fakeslack.WithPingInterval(0))
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	errPing := errors.New("ping")
	ws.SetPingHandler(func(string) error { return errPing })
	_ = ws.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err := ws.ReadMessage() // hello
	if err == nil {
		_, _, err = ws.ReadMessage()
	}
	if errors.Is(err, errPing) || err == nil {
		t.Fatalf("read after hello = %v; want a timeout, since pings are off", err)
	}
}

func TestSocketModePingsClient(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New(fakeslack.WithPingInterval(10 * time.Millisecond))
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	errPing := errors.New("ping")
	ws.SetPingHandler(func(string) error { return errPing })
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for range 2 { // hello, then the ping surfaces as the handler's error
		if _, _, err := ws.ReadMessage(); errors.Is(err, errPing) {
			return
		}
	}
	t.Fatal("no websocket ping from the fake; slack-go's client would give up after 30s")
}
