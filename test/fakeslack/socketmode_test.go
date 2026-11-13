package fakeslack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/testutil"
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
	testutil.Eventually(t, "the connection", func() bool { return fake.Connections() == 1 }, testutil.Every(5*time.Millisecond))

	_ = ws.Close()
	testutil.Eventually(t, "the disconnect", func() bool { return fake.Connections() == 0 }, testutil.Every(5*time.Millisecond))
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

// A dropped connection is closed with no disconnect envelope and no close frame, as a network that fails does: the
// client sees its socket end, which is not what a refresh looks like, and (as slack-go does) connects again.
func TestDropConnectionsClosesTheSocketWithoutTellingTheClient(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	app := fake.Apps()[0].AppID

	n, err := fake.DropConnections(app)

	if err != nil || n != 1 {
		t.Fatalf("DropConnections = %d, %v; want 1 connection dropped", n, err)
	}
	// No look at AppConnections here: the client may connect again before it, as DropConnections says nothing prevents.
	// The dropped connection is counted by n, and the second hello and AppConnectionsOpened below show the reconnection.
	var seen []socketmode.EventType
	timeout := time.After(30 * time.Second)
	for second := false; !second; {
		select {
		case ev := <-smc.Events:
			seen = append(seen, ev.Type)
			second = ev.Type == socketmode.EventTypeHello
		case <-timeout:
			t.Fatalf("the client did not connect again; it saw %v", seen)
		}
	}
	if slices.Contains(seen, socketmode.EventTypeDisconnect) {
		t.Errorf("the client saw %v; a drop sends no disconnect envelope", seen)
	}
	if got := fake.AppConnectionsOpened(app); got != 2 {
		t.Errorf("AppConnectionsOpened = %d; want the first connection and the one after the drop", got)
	}
}

func TestDropConnectionsOfAnAppTheServerDoesNotHost(t *testing.T) {
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	if n, err := fake.DropConnections("ANOSUCH"); !errors.Is(err, fakeslack.ErrNotFound) || n != 0 {
		t.Errorf("DropConnections = %d, %v; want 0 and ErrNotFound", n, err)
	}
}

// A drop of one app leaves the connections of another.
func TestDropConnectionsLeavesOtherApps(t *testing.T) {
	appA := fakeslack.App{BotUser: "UA", BotID: "BA", AppID: "AA", BotToken: "xoxb-a", AppToken: "xapp-a"}
	appB := fakeslack.App{BotUser: "UB", BotID: "BB", AppID: "AB", BotToken: "xoxb-b", AppToken: "xapp-b"}
	fake := fakeslack.New(fakeslack.WithApp(appA), fakeslack.WithApp(appB))
	t.Cleanup(fake.Close)
	for _, a := range []fakeslack.App{appA, appB} {
		api := slack.New(a.BotToken, slack.OptionAPIURL(fake.URL()+"/"), slack.OptionAppLevelToken(a.AppToken))
		_, u, err := api.StartSocketModeContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ws, resp, err := websocket.DefaultDialer.DialContext(t.Context(), u, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		t.Cleanup(func() { _ = ws.Close() })
	}
	testutil.Eventually(t, "both apps to connect", func() bool { return fake.Connections() == 2 }, testutil.Every(5*time.Millisecond))

	if _, err := fake.DropConnections("AA"); err != nil {
		t.Fatal(err)
	}
	if a, b := fake.AppConnections("AA"), fake.AppConnections("AB"); a != 0 || b != 1 {
		t.Errorf("connections of A and B = %d and %d after dropping A; want 0 and 1", a, b)
	}
}
