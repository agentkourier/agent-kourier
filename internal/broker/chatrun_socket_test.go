package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// These tests run RunPlatform over the real Slack adapter against the fake Slack, with a clock they step, so that the
// pause of a reconnect is read and not waited for. What they pin is who reconnects a dropped socket: slack-go reconnects
// inside Run with a pause that grows to 5 minutes, and the broker's policy (1 s doubling to 60 s, cut by jitter) must
// be the one that applies.

// reconnectGate fronts a fake Slack the way an outage looks from the broker: while it is set, apps.connections.open
// fails, so a socket that was dropped cannot be opened again, and every other call goes through. A refusal is a
// single response, token_expired for example, that the gate answers apps.connections.open with instead.
type reconnectGate struct {
	inner   http.Handler
	down    atomic.Bool
	refusal atomic.Pointer[string]
	opens   atomic.Int32 // apps.connections.open calls that reached the gate
}

func (g *reconnectGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/apps.connections.open" {
		g.inner.ServeHTTP(w, r)
		return
	}
	g.opens.Add(1)
	if code := g.refusal.Load(); code != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": *code})
		return
	}
	if g.down.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	g.inner.ServeHTTP(w, r)
}

// steppedClock is the After of a RunPolicy that waits for the test: each wait is reported on calls and ends when the test fires it.
type steppedClock struct{ calls chan pause }

type pause struct {
	d    time.Duration
	fire chan time.Time
}

func newSteppedClock() *steppedClock { return &steppedClock{calls: make(chan pause, 64)} }

func (c *steppedClock) After(d time.Duration) <-chan time.Time {
	p := pause{d: d, fire: make(chan time.Time, 1)}
	c.calls <- p
	return p.fire
}

// next waits for RunPlatform to wait, and returns the wait. It fails the test if RunPlatform does not within 10 s: the
// adapter went on by itself instead of handing the retry back.
func (c *steppedClock) next(t *testing.T, what string) pause {
	t.Helper()
	select {
	case p := <-c.calls:
		return p
	case <-time.After(10 * time.Second):
		t.Fatalf("RunPlatform did not wait before connecting again %s", what)
		return pause{}
	}
}

// recordedSink is a chat.Sink that records the messages it takes. A message it is given while hold is set waits for the
// context and is not taken (the Sink fails it with the context's error, which leaves the event unacknowledged).
type recordedSink struct {
	mu      sync.Mutex
	handled map[string]int // by message ID: how many times Message returned nil
	entered chan string    // the ID of each message a held Sink was given
	hold    atomic.Bool
}

func newRecordedSink() *recordedSink {
	return &recordedSink{handled: map[string]int{}, entered: make(chan string, 16)}
}

func (s *recordedSink) Message(ctx context.Context, m chat.Inbound) error {
	if s.hold.Load() {
		s.entered <- m.At.ID
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	s.handled[m.At.ID]++
	s.mu.Unlock()
	return nil
}

func (s *recordedSink) Interaction(context.Context, chat.Interaction) error { return nil }

func (s *recordedSink) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handled[id]
}

// connStates records what the adapter tells of its connection, which is what agentkourier_chat_connected shows.
type connStates struct {
	mu  sync.Mutex
	got []bool
}

func (s *connStates) set(up bool) { s.mu.Lock(); s.got = append(s.got, up); s.mu.Unlock() }

func (s *connStates) get() []bool { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.got) }

// socketRun is a RunPlatform over a Slack client of one app of the fake, run until the test ends.
type socketRun struct {
	app    fakeslack.App
	clock  *steppedClock
	sink   *recordedSink
	states *connStates
	done   chan struct{} // closed when RunPlatform has returned, with err
	err    error
}

// socketWorld is a fake Slack behind a gate, with the apps it hosts.
type socketWorld struct {
	fake *fakeslack.Server
	gate *reconnectGate
	srv  *httptest.Server
}

func newSocketWorld(t *testing.T, apps ...fakeslack.App) *socketWorld {
	t.Helper()
	before := goleak.IgnoreCurrent()
	opts := make([]fakeslack.Option, 0, len(apps))
	for _, a := range apps {
		opts = append(opts, fakeslack.WithApp(a))
	}
	fake := fakeslack.NewHandler(opts...)
	g := &reconnectGate{inner: fake}
	srv := httptest.NewServer(g)
	t.Cleanup(func() {
		fake.Close()
		srv.Close()
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	return &socketWorld{fake: fake, gate: g, srv: srv}
}

func testApp(n int) fakeslack.App {
	return fakeslack.App{
		BotUser: fmt.Sprintf("UBOT%d", n), BotID: fmt.Sprintf("BBOT%d", n), AppID: fmt.Sprintf("AAPP%d", n),
		BotToken: fmt.Sprintf("xoxb-fake-%d", n), AppToken: fmt.Sprintf("xapp-fake-%d", n),
	}
}

// run starts RunPlatform for app with a random source seeded by seed (a cut of 0 when it is negative), and returns once the app is connected.
func (w *socketWorld) run(t *testing.T, app fakeslack.App, seed int) *socketRun {
	t.Helper()
	r := &socketRun{app: app, clock: newSteppedClock(), sink: newRecordedSink(), states: &connStates{}, done: make(chan struct{})}
	client := slack.New(slack.Config{
		BotToken: secret.New(app.BotToken), AppToken: secret.New(app.AppToken), APIURL: w.srv.URL + "/",
		Logger: testutil.DiscardLogger(), OnConnection: r.states.set,
	})
	random := func() float64 { return 0 }
	if seed >= 0 {
		random = rand.New(rand.NewPCG(uint64(seed), 1)).Float64
	}
	platform := slack.NewPlatform(client, "ns/"+app.AppID)
	ctx, cancel := context.WithCancel(t.Context())
	// Now stays where it is: the connection never counts as having been up for ResetAfter, so the pause only grows.
	frozen := time.Unix(1_700_000_000, 0)
	policy := RunPolicy{Now: func() time.Time { return frozen }, After: r.clock.After, Random: random, Logger: testutil.DiscardLogger()}
	go func() { r.err = RunPlatform(ctx, platform, r.sink, policy); close(r.done) }()
	t.Cleanup(func() { cancel(); <-r.done })
	w.waitConnections(t, app, 1)
	testutil.Eventually(t, "the adapter to take Slack's hello", func() bool { return slices.Equal(r.states.get(), []bool{true}) }, testutil.Within(10*time.Second))
	return r
}

// waitConnections waits until app has n Socket Mode clients connected.
func (w *socketWorld) waitConnections(t *testing.T, app fakeslack.App, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); w.fake.AppConnections(app.AppID) != n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("app %s has %d connections after 10 s; want %d", app.AppID, w.fake.AppConnections(app.AppID), n)
		}
	}
}

// event is a message event in channel C1 with the timestamp ts.
func event(ts string) map[string]any {
	return map[string]any{"type": "message", "user": "U1", "text": "hello", "ts": ts, "event_ts": ts, "channel": "C1", "channel_type": "channel"}
}

func (w *socketWorld) send(t *testing.T, app fakeslack.App, ts string) string {
	t.Helper()
	id, err := w.fake.SendEnvelopeTo(app.AppID, "events_api", map[string]any{
		"type": "event_callback", "team_id": "TFAKE", "api_app_id": app.AppID, "event_id": "Ev" + ts, "event": event(ts),
	})
	if err != nil {
		t.Fatalf("sending the event %s: %v", ts, err)
	}
	return id
}

func (w *socketWorld) waitAck(t *testing.T, ids ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := w.fake.WaitAck(ctx, ids...); err != nil {
		t.Fatalf("the events %v were not acknowledged: %v", ids, err)
	}
}

// A socket that Slack drops while it is up, with Slack then unreachable, is reconnected on RunPlatform's schedule: it
// waits 1 s, then 2 s, doubling to the 60 s cap and never past it, though slack-go's own schedule would have been
// waiting for as long as 5 minutes by then. When Slack is back the next attempt connects.
func TestADroppedSocketIsReconnectedOnRunPlatformsSchedule(t *testing.T) {
	app := testApp(1)
	w := newSocketWorld(t, app)
	r := w.run(t, app, -1)

	w.gate.down.Store(true)
	if n, err := w.fake.DropConnections(app.AppID); err != nil || n != 1 {
		t.Fatalf("DropConnections = %d, %v", n, err)
	}

	var waits []time.Duration
	var last pause
	for i := range 8 {
		last = r.clock.next(t, fmt.Sprintf("after %d failed attempts of an outage", i))
		waits = append(waits, last.d)
		if i < 7 {
			last.fire <- time.Time{}
		}
	}
	if want := secs(1, 2, 4, 8, 16, 32, 60, 60); !slices.Equal(waits, want) {
		t.Errorf("waits = %v; want %v: RunPlatform's schedule, capped at 60 s", waits, want)
	}
	if got := r.states.get(); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("the connection was reported %v during the outage; want up, then down and no more", got)
	}

	w.gate.down.Store(false)
	last.fire <- time.Time{}
	w.waitConnections(t, app, 1)
	testutil.Eventually(t, "the connection to be reported up again", func() bool { return slices.Equal(r.states.get(), []bool{true, false, true}) }, testutil.Within(10*time.Second))
}

// Connections that are dropped at once and cannot reconnect do not retry at the same moment: each waits a time of
// its own within the pause.
func TestManyDroppedSocketsReconnectAtJitteredTimes(t *testing.T) {
	const n = 8
	apps := make([]fakeslack.App, n)
	for i := range apps {
		apps[i] = testApp(i)
	}
	w := newSocketWorld(t, apps...)
	runs := make([]*socketRun, n)
	for i, a := range apps {
		runs[i] = w.run(t, a, i)
	}

	w.gate.down.Store(true)
	for _, a := range apps {
		if _, err := w.fake.DropConnections(a.AppID); err != nil {
			t.Fatal(err)
		}
	}

	waited := map[time.Duration]bool{}
	for i, r := range runs {
		p := r.clock.next(t, fmt.Sprintf("for app %d", i))
		if p.d > time.Second || p.d < 500*time.Millisecond {
			t.Errorf("app %d waits %v; want between 500ms and 1s", i, p.d)
		}
		waited[p.d] = true
	}
	if len(waited) < n*3/4 {
		t.Errorf("%d connections dropped together wait %d different times; want them spread", n, len(waited))
	}
}

// A credential Slack refuses on a reconnect is as fatal as at the start: RunPlatform returns the rejection at once, to
// exit the process (the own connection) or stop that connection (a team's), and waits for nothing. token_revoked is what
// slack-go itself treats as fatal; token_expired it retries without end, which RunPlatform must not leave it to do.
func TestARefusalOnAReconnectIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(w *socketWorld, app fakeslack.App)
		code   string
	}{
		{"token_revoked", func(w *socketWorld, app fakeslack.App) {
			if err := w.fake.SetRevoked(app.AppID, true); err != nil {
				t.Fatal(err)
			}
		}, "token_revoked"},
		{"token_expired", func(w *socketWorld, app fakeslack.App) {
			code := "token_expired"
			w.gate.refusal.Store(&code)
			if _, err := w.fake.DropConnections(app.AppID); err != nil {
				t.Fatal(err)
			}
		}, "token_expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(1)
			w := newSocketWorld(t, app)
			r := w.run(t, app, -1)

			tc.refuse(w, app)

			select {
			case <-r.done:
				var rej *authRejected
				if !errors.Is(r.err, ErrSlackAuth) || !errors.As(r.err, &rej) || rej.code != tc.code {
					t.Errorf("RunPlatform = %v; want an authRejected with %s", r.err, tc.code)
				}
			case p := <-r.clock.calls:
				t.Fatalf("RunPlatform waited %v to connect again; want it to stop at once", p.d)
			case <-time.After(10 * time.Second):
				t.Fatal("RunPlatform did not return: a refused credential was retried without end")
			}
		})
	}
}

// An event is taken once across a reconnect. One acknowledged before the drop is not given again, one that was
// in the sink's hands when the connection broke is not acknowledged (the sink did not take it), and when Slack sends it
// again on the new connection, as it does for what it was not told it was taken, the sink takes it once.
func TestNoEventIsTakenTwiceAcrossAReconnect(t *testing.T) {
	app := testApp(1)
	w := newSocketWorld(t, app)
	r := w.run(t, app, -1)
	before := w.send(t, app, "1.1")
	w.waitAck(t, before)

	r.sink.hold.Store(true)
	inFlight := w.send(t, app, "2.2")
	if got := <-r.sink.entered; got != "2.2" {
		t.Fatalf("the sink was given %s; want 2.2", got)
	}
	w.gate.down.Store(true)
	if _, err := w.fake.DropConnections(app.AppID); err != nil {
		t.Fatal(err)
	}
	p := r.clock.next(t, "after the drop")
	if _, acked := w.fake.Ack(inFlight); acked {
		t.Error("the event the sink had not taken was acknowledged")
	}

	r.sink.hold.Store(false)
	w.gate.down.Store(false)
	p.fire <- time.Time{}
	w.waitConnections(t, app, 1)
	again := w.send(t, app, "2.2") // Slack's redelivery of the event it never saw acknowledged
	after := w.send(t, app, "3.3")
	w.waitAck(t, again, after)

	for ts, want := range map[string]int{"1.1": 1, "2.2": 1, "3.3": 1} {
		if got := r.sink.count(ts); got != want {
			t.Errorf("event %s was taken %d times; want %d", ts, got, want)
		}
	}
}
