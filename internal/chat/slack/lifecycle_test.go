package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// ticket stands for the ticket in the URL that apps.connections.open returns, which opens a connection for the app.
const ticket = "TICKET-0f9e8d7c"

// userText is what a person wrote in the envelopes these tests send, and no log may hold it.
const userText = "my password is hunter2"

// syncBuffer is a log destination that a test may read while the logger writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// count is how many lines of the log hold every one of subs.
func (s *syncBuffer) count(subs ...string) int {
	n := 0
	for _, l := range strings.Split(s.String(), "\n") {
		all := true
		for _, sub := range subs {
			all = all && strings.Contains(l, sub)
		}
		if all {
			n++
		}
	}
	return n
}

// states records what OnConnection is told.
type states struct {
	mu  sync.Mutex
	got []bool
}

func (s *states) set(up bool) { s.mu.Lock(); s.got = append(s.got, up); s.mu.Unlock() }

func (s *states) get() []bool { s.mu.Lock(); defer s.mu.Unlock(); return append([]bool(nil), s.got...) }

// last is the latest state, false before any.
func (s *states) last() bool { g := s.get(); return len(g) > 0 && g[len(g)-1] }

// gate fronts a fake Slack the way Slack's own edge differs from it: the URL that apps.connections.open returns
// carries a ticket, and a test can make that call fail.
type gate struct {
	inner    http.Handler
	failOpen atomic.Bool
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/apps.connections.open" {
		g.inner.ServeHTTP(w, r)
		return
	}
	if g.failOpen.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	rec := httptest.NewRecorder()
	g.inner.ServeHTTP(rec, r)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u, ok := body["url"].(string); ok {
		body["url"] = u + "?ticket=" + ticket
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(body)
}

// watched is a Client running against a fake Slack behind a gate.
type watched struct {
	fake   *fakeslack.Server
	gate   *gate
	logs   *syncBuffer
	states *states
}

// startWatched runs a Client with a debug log and an OnConnection recorder, and returns once it has been told it is
// connected. The end of the test cancels Run and checks that nothing is left running.
func startWatched(t *testing.T) *watched {
	t.Helper()
	before := goleak.IgnoreCurrent()
	fake := fakeslack.NewHandler()
	g := &gate{inner: fake}
	srv := httptest.NewServer(g)
	t.Cleanup(func() {
		fake.Close()
		srv.Close()
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	k := &watched{fake: fake, gate: g, logs: &syncBuffer{}, states: &states{}}
	c := slack.New(slack.Config{
		BotToken: secret.New(botToken), AppToken: secret.New(appToken), APIURL: srv.URL + "/",
		Logger:       slog.New(slog.NewTextHandler(k.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnConnection: k.states.set,
	})
	run(t, c, &sink{})
	k.eventually(t, "the client to be told it is connected", k.states.last)
	// The state is set before the line is logged, so being told says nothing yet of the log.
	k.eventually(t, "the connection to be logged", func() bool { return k.logs.count("level=INFO", "socket mode connected") > 0 })
	return k
}

// eventually waits up to 10 seconds for cond.
func (k *watched) eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; the log:\n%s", what, k.logs)
		}
	}
}

// noSecrets fails the test if the log holds a token, the ticket or the socket's URL, or what a person wrote.
func (k *watched) noSecrets(t *testing.T) {
	t.Helper()
	logs := k.logs.String()
	for _, bad := range []string{botToken, appToken, ticket, "ticket=", "ws://", "wss://", userText} {
		if strings.Contains(logs, bad) {
			t.Errorf("the log holds %q:\n%s", bad, logs)
		}
	}
}

// A connection says so when it is made: Socket Mode connecting, the websocket opened and Slack's hello, once each,
// and OnConnection hears of it. An envelope shows at DEBUG by its type and ID, with nothing of what it carried.
func TestConnectionIsLoggedAndReported(t *testing.T) {
	k := startWatched(t)

	if got := k.states.get(); len(got) != 1 || !got[0] {
		t.Errorf("OnConnection was told %v; want one true", got)
	}
	for _, want := range [][]string{
		{"level=INFO", "socket mode connecting"},
		{"level=INFO", "socket mode websocket opened"},
		{"level=INFO", "socket mode connected", "num_connections=1"},
	} {
		if got := k.logs.count(want...); got != 1 {
			t.Errorf("%d log lines hold %q; want 1:\n%s", got, want, k.logs)
		}
	}

	m, err := k.fake.Mention("C1", "U1", botMark+" "+userText)
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, k.fake, m.Envelopes...)
	if got := k.logs.count("level=DEBUG", "slack: envelope", "events_api", "envelope_id="+m.Envelopes[0]); got != 1 {
		t.Errorf("%d DEBUG lines name the envelope %s; want 1:\n%s", got, m.Envelopes[0], k.logs)
	}
	k.noSecrets(t)
}

// A connection that is lost is a gauge that drops while Slack cannot be reached, with a WARN for each failed attempt
// (once, however often it is retried), and one that is up again when it can: the second connection is logged like the
// first.
func TestLostConnectionIsReportedAndRecovers(t *testing.T) {
	k := startWatched(t)
	k.gate.failOpen.Store(true)
	if _, err := k.fake.SendEnvelope("disconnect", map[string]any{"reason": "refresh_requested"}); err != nil {
		t.Fatal(err)
	}

	k.eventually(t, "the connection to be reported down", func() bool { return !k.states.last() })
	k.eventually(t, "the failed attempt to be logged", func() bool { return k.logs.count("level=WARN", "connection attempt failed") > 0 })
	if got := k.logs.count("level=INFO", "socket mode reconnecting"); got != 1 {
		t.Errorf("%d lines say it was reconnecting; want 1:\n%s", got, k.logs)
	}

	k.gate.failOpen.Store(false)
	k.eventually(t, "the connection to be reported up again", k.states.last)
	// The state is set before the line is logged: wait for the second line, then count.
	k.eventually(t, "the second connection to be logged", func() bool { return k.logs.count("level=INFO", "socket mode connected") >= 2 })
	if got := k.states.get(); len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Errorf("OnConnection was told %v; want up, down, up", got)
	}
	if got := k.logs.count("level=INFO", "socket mode connected"); got != 2 {
		t.Errorf("%d lines say it connected; want 2 (the first and the second):\n%s", got, k.logs)
	}
	if got := k.logs.count("connection attempt failed"); got != 1 {
		t.Errorf("%d lines for the failed attempts; want the few that the pace allows, which here is 1:\n%s", got, k.logs)
	}
	k.noSecrets(t)
}

// A refresh, which Slack asks for with a disconnect and which slack-go answers by closing the socket and connecting
// again, is routine: the connection goes down and up, and nothing is a WARN, though slack-go reports a read on the
// socket it closed.
func TestRefreshReconnectsWithoutAWarning(t *testing.T) {
	k := startWatched(t)

	if _, err := k.fake.SendEnvelope("disconnect", map[string]any{"reason": "refresh_requested"}); err != nil {
		t.Fatal(err)
	}

	k.eventually(t, "the second hello", func() bool { return k.logs.count("level=INFO", "socket mode connected") == 2 })
	if got := k.states.get(); len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Errorf("OnConnection was told %v; want up, down, up", got)
	}
	if got := k.logs.count("level=WARN"); got != 0 {
		t.Errorf("a refresh logged %d WARN lines:\n%s", got, k.logs)
	}
	k.noSecrets(t)
}

// A message that cannot be read is a WARN, and the connection stays up. Nothing of it is logged but its size.
func TestUnreadableMessageIsWarned(t *testing.T) {
	k := startWatched(t)

	if _, err := k.fake.SendEnvelope("a_type_nobody_defined", map[string]any{"text": userText}); err != nil {
		t.Fatal(err)
	}

	k.eventually(t, "the WARN", func() bool {
		return k.logs.count("level=WARN", "message could not be read", "a_type_nobody_defined") > 0
	})
	if got := k.states.get(); len(got) != 1 {
		t.Errorf("OnConnection was told %v; a message that cannot be read does not drop the connection", got)
	}
	k.noSecrets(t)
}

// Run ending is the connection down, whatever ended it.
func TestRunEndingReportsTheConnectionDown(t *testing.T) {
	fake := newFake(t)
	var st states
	cfg := clientConfig(fake)
	cfg.OnConnection = st.set
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- slack.New(cfg).Run(ctx, &sink{}) }()
	waitConnected(t, fake)
	for deadline := time.Now().Add(5 * time.Second); !st.last(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the client was never told it is connected")
		}
	}

	cancel()
	<-done

	if got := st.get(); len(got) != 2 || !got[0] || got[1] {
		t.Errorf("OnConnection was told %v; want up, then down when Run ended", got)
	}
}
