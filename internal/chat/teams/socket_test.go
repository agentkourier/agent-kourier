package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/faketeams"
)

// socketTest is a Client of the test connection in socket mode, on the fake Socket mode service and the fake Entra, with what it
// reports. Its waits are short, and its jitter none.
type socketTest struct {
	c       *Client
	f       *faketeams.SocketMode
	entra   *fakeEntra
	sink    *recordingSink
	obs     observed
	logs    *syncBuffer
	invokes *blockingInvokes

	mu        sync.Mutex
	connected []bool // what OnConnection was told, in order
	gaps      []Gap  // what OnSocketGap was told, in order

	cancel context.CancelFunc
	done   chan error
}

// syncBuffer is a log's text, written by several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingInvokes answers each invoke with answer, after release is closed or the invoke's context ends, if release is set.
type blockingInvokes struct {
	mu      sync.Mutex
	answer  InvokeAnswer
	release chan struct{}
	started chan struct{} // gets a value as each invoke starts, if set
}

func (b *blockingInvokes) Invoke(ctx context.Context, _ chat.Sink, _ Invoke) InvokeAnswer {
	b.mu.Lock()
	release, started, ans := b.release, b.started, b.answer
	b.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	return ans
}

// newSocketTest makes the Client, in the regions given (amer alone if none), and does not run it.
func newSocketTest(t *testing.T, regions ...string) *socketTest {
	t.Helper()
	if len(regions) == 0 {
		regions = []string{"amer"}
	}
	st := &socketTest{f: faketeams.New(t), entra: newFakeEntra(t), sink: &recordingSink{}, obs: newObserved(t), logs: &syncBuffer{},
		invokes: &blockingInvokes{answer: InvokeAnswer{Status: http.StatusOK, Body: []byte(`{"statusCode":200,"type":"application/vnd.microsoft.activity.message","value":"Recorded"}`)}}}
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: st.entra.tokenURL(), HTTPClient: st.f.Client(), Logger: slog.New(slog.NewTextHandler(st.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Socket: true, SocketURL: st.f.URL, SocketTLS: st.f.TLS(),
		OnConnection: func(up bool) { st.mu.Lock(); st.connected = append(st.connected, up); st.mu.Unlock() },
		OnSocketGap:  func(g Gap) { st.mu.Lock(); st.gaps = append(st.gaps, g); st.mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	c.recv.invokes = st.invokes
	s := c.sockets
	s.regions = regions
	s.retryBase, s.retryMax, s.refusedRetry, s.reconnectNow = 10*time.Millisecond, 40*time.Millisecond, time.Hour, time.Hour
	s.retireAfter, s.readyWait = 50*time.Millisecond, 5*time.Second
	s.jitter = func() float64 { return 0.5 } // no jitter
	st.c = c
	return st
}

// run runs the Client until stop or the test's end.
func (st *socketTest) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(st.obs.ctx)
	st.cancel, st.done = cancel, make(chan error, 1)
	go func() { st.done <- st.c.Run(ctx, st.sink) }()
	t.Cleanup(func() { _ = st.stop(t) })
}

// stop ends Run and returns its error.
func (st *socketTest) stop(t *testing.T) error {
	t.Helper()
	st.cancel()
	select {
	case err := <-st.done:
		st.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
		return nil
	}
}

// ended waits for Run to return by itself, and returns its error.
func (st *socketTest) ended(t *testing.T) error {
	t.Helper()
	select {
	case err := <-st.done:
		st.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func (st *socketTest) told() ([]bool, []Gap) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]bool(nil), st.connected...), append([]Gap(nil), st.gaps...)
}

// up waits until the connection was last told it is connected, or not.
func (st *socketTest) up(t *testing.T, want bool) {
	t.Helper()
	testutil.Eventually(t, "OnConnection to be told the connection's state", func() bool {
		c, _ := st.told()
		return len(c) > 0 && c[len(c)-1] == want
	})
}

// series is the value of the series of name under the test connection and the other labels given, of a counter, a gauge or the
// count of a histogram; 0 when it has none.
func (st *socketTest) series(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	mfs, err := st.obs.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			have := map[string]bool{}
			conn := false
			for _, l := range m.GetLabel() {
				have[l.GetValue()] = true
				conn = conn || l.GetName() == telemetry.ConnectionLabel && l.GetValue() == testConn
			}
			if !conn {
				continue
			}
			for _, l := range labels {
				if !have[l] {
					continue metrics
				}
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// envelope is the fixture name as an envelope of typ, with the deadline given (25 s if 0).
func envelopeOf(t *testing.T, typ, id string, payload []byte, deadlineMs int) faketeams.Envelope {
	t.Helper()
	if deadlineMs == 0 {
		deadlineMs = 25000
	}
	return faketeams.Envelope{Type: typ, EnvelopeID: id, DeadlineMs: deadlineMs, AckRequired: true, Payload: json.RawMessage(payload)}
}

// result waits for the frame of the call id on sock.
func result(t *testing.T, sock *faketeams.Socket, id string) faketeams.Frame {
	t.Helper()
	fr, ok := sock.Result(id, 10*time.Second)
	if !ok {
		t.Fatalf("no reply frame for the call %s on %s", id, sock)
	}
	return fr
}

// A connection in socket mode offers the chat listener no handler; it connects with the bot's token from Entra, makes SignalR's
// handshake through the negotiate's redirect, is up once the service calls SocketReady, and answers a message's envelope with a reply
// frame of 200 at once, before the sink has taken the post, which it then does.
func TestSocketModeTakesAMessageAndAcknowledgesItAtOnce(t *testing.T) {
	st := newSocketTest(t)
	if _, ok := st.c.HTTPHandler(); ok {
		t.Error("a connection in socket mode offers a handler for the chat listener; want none")
	}
	st.sink.block = make(chan struct{})
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	st.up(t, true)
	if got := st.f.Connects()[0].Authorization; got != "Bearer fake-token-1" {
		t.Errorf("the connect call carried %q; want the bot's token from Entra", got)
	}
	if got := st.series(t, "agentkourier_teams_socket_ready", "amer"); got != 1 {
		t.Errorf("agentkourier_teams_socket_ready{region=amer} = %v; want 1", got)
	}

	id := sock.Activity(t, envelopeOf(t, "activity", "env-1", fixture(t, "message_plain"), 0))
	fr := result(t, sock, id)
	if fr.Status != http.StatusOK || fr.EnvelopeID != "env-1" || fr.ProtocolVersion != 1 || fr.RecvAt == 0 || fr.TS < fr.RecvAt || len(fr.Body) != 0 {
		t.Errorf("the reply frame = %+v; want status 200 for env-1, protocol 1, its times, and no body", fr)
	}
	if len(st.sink.got()) != 0 {
		t.Error("the sink had the post before the frame was answered; want the answer first, as over https")
	}
	close(st.sink.block)
	testutil.Eventually(t, "the sink to take the post", func() bool { return len(st.sink.got()) == 1 })
	if got := st.series(t, "agentkourier_teams_socket_envelopes_total", "activity", "accepted"); got != 1 {
		t.Errorf("envelopes{activity,accepted} = %v; want 1", got)
	}
	if got := st.series(t, "agentkourier_teams_requests_total", "accepted"); got != 0 {
		t.Errorf("requests_total{accepted} = %v; want 0: an envelope is no request over https", got)
	}
}

// An envelope is worked on as a request over https is, without a token: a duplicate, another tenant, something that is no activity,
// and an envelope of a type the adapter does not know each have the answer and outcome they have over https.
func TestSocketModeAnswersEachEnvelopeAsHTTPSWould(t *testing.T) {
	st := newSocketTest(t)
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	st.up(t, true)
	other := editFixture(t, "message_plain", func(m map[string]any) {
		m["channelData"].(map[string]any)["tenant"] = map[string]any{"id": "99999999-2222-4333-8444-555555555555"}
	})
	tests := []struct {
		name, typ   string
		payload     []byte
		wantStatus  int
		wantOutcome string
	}{
		{"a message", "activity", fixture(t, "message_mention"), http.StatusOK, "accepted"},
		{"the same message again", "activity", fixture(t, "message_mention"), http.StatusOK, "duplicate"},
		{"another tenant", "activity", other, http.StatusForbidden, "wrong_tenant"},
		{"no activity", "activity", []byte(`{"type":7}`), http.StatusBadRequest, "malformed"},
		{"a type it does not know", "notify", fixture(t, "message_reply"), http.StatusBadRequest, "malformed"},
	}
	for _, tc := range tests {
		id := sock.Activity(t, envelopeOf(t, tc.typ, "env-"+tc.name, tc.payload, 0))
		if fr := result(t, sock, id); fr.Status != tc.wantStatus {
			t.Errorf("%s: status %d; want %d", tc.name, fr.Status, tc.wantStatus)
		}
		typ := envelopeType(tc.typ)
		testutil.Eventually(t, "the envelope to be counted", func() bool {
			return st.series(t, "agentkourier_teams_socket_envelopes_total", typ, tc.wantOutcome) >= 1
		})
	}
}

// An invoke is answered in its reply frame with the InvokeHandler's answer as the frame's body.
func TestSocketModeAnswersAnInvokeInItsFrame(t *testing.T) {
	st := newSocketTest(t)
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	id := sock.Activity(t, envelopeOf(t, "invoke", "env-press", fixture(t, "invoke_action"), 0))
	fr := result(t, sock, id)
	if fr.Status != http.StatusOK || string(fr.Body) != string(st.invokes.answer.Body) {
		t.Errorf("the invoke's frame = status %d body %s; want 200 and the handler's answer", fr.Status, fr.Body)
	}
	if got := st.series(t, "agentkourier_teams_socket_envelopes_total", "invoke", "invoke"); got != 1 {
		t.Errorf("envelopes{invoke,invoke} = %v; want 1", got)
	}
}

// An invoke whose handler is still deciding is answered before the envelope's deadline: its context ends a margin before it.
func TestSocketModeAnswersAnInvokeWithinItsDeadline(t *testing.T) {
	st := newSocketTest(t)
	st.invokes.release = make(chan struct{}) // never released: the handler answers when its context ends
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	const deadline = 1500 * time.Millisecond
	sent := time.Now()
	id := sock.Activity(t, envelopeOf(t, "invoke", "env-slow", fixture(t, "invoke_action"), int(deadline.Milliseconds())))
	fr := result(t, sock, id)
	took := time.Since(sent)
	if took >= deadline || took < deadline-socketReplyMargin-200*time.Millisecond {
		t.Errorf("the frame came %s after the invoke; want it about %s, before the deadline of %s", took, deadline-socketReplyMargin, deadline)
	}
	if fr.Status != http.StatusOK || fr.TS-fr.RecvAt >= deadline.Milliseconds() {
		t.Errorf("the frame = %+v; want 200 within the deadline", fr)
	}
}

// Past socketMaxInFlight envelopes being handled, one more is answered 503 at once and counted busy.
func TestSocketModeAnswersBusyPastItsPlaces(t *testing.T) {
	st := newSocketTest(t)
	st.c.sockets.slots = make(chan struct{}, 1)
	st.invokes.release, st.invokes.started = make(chan struct{}), make(chan struct{}, 1)
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	held := sock.Activity(t, envelopeOf(t, "invoke", "env-held", fixture(t, "invoke_action"), 0))
	<-st.invokes.started
	id := sock.Activity(t, envelopeOf(t, "activity", "env-busy", fixture(t, "message_plain"), 0))
	if fr := result(t, sock, id); fr.Status != http.StatusServiceUnavailable {
		t.Errorf("the envelope past the places was answered %d; want 503", fr.Status)
	}
	if got := st.series(t, "agentkourier_teams_socket_envelopes_total", "activity", "busy"); got != 1 {
		t.Errorf("envelopes{activity,busy} = %v; want 1", got)
	}
	close(st.invokes.release)
	if fr := result(t, sock, held); fr.Status != http.StatusOK {
		t.Errorf("the held invoke was answered %d; want 200", fr.Status)
	}
}

// Before its token expires a socket is replaced: a new connect call, with a token of its own, names the old socket's connection ID; the
// old socket stays until the envelope it is handling is answered on it, and is then closed by the client. The connection stays up
// throughout, with no gap.
func TestSocketModeReplacesASocketBeforeItsTokenExpires(t *testing.T) {
	st := newSocketTest(t)
	st.f.ExpireAfter(2) // replaced at half its life, a second in
	st.invokes.release, st.invokes.started = make(chan struct{}), make(chan struct{}, 1)
	st.run(t)
	old := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	st.up(t, true)
	held := old.Activity(t, envelopeOf(t, "invoke", "env-inflight", fixture(t, "invoke_action"), 0))
	<-st.invokes.started

	next := st.f.WaitSocket(t, "amer", 1, 5*time.Second)
	if next.Replaces != old.ConnectionID {
		t.Errorf("the replacement named %q as the socket it replaces; want %q", next.Replaces, old.ConnectionID)
	}
	time.Sleep(3 * st.c.sockets.retireAfter)
	select {
	case <-old.Closed():
		t.Fatal("the old socket was closed while it handled an envelope; want it kept until the reply is sent")
	default:
	}
	close(st.invokes.release)
	if fr := result(t, old, held); fr.Status != http.StatusOK {
		t.Errorf("the envelope in flight on the old socket was answered %d; want 200 on the old socket", fr.Status)
	}
	select {
	case <-old.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the old socket was not closed after its reply")
	}
	if !old.ClosedByClient() {
		t.Error("the old socket was not closed by the client")
	}
	connects := st.f.RegionConnects("amer")
	if len(connects) < 2 || connects[1].Replaces != old.ConnectionID {
		t.Errorf("connects = %+v; want a second that names the old socket", connects)
	}
	if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "amer", "expiry"); got < 1 {
		t.Errorf("reconnects{amer,expiry} = %v; want at least 1", got)
	}
	// The new socket takes envelopes.
	id := next.Activity(t, envelopeOf(t, "activity", "env-next", fixture(t, "message_plain"), 0))
	if fr := result(t, next, id); fr.Status != http.StatusOK {
		t.Errorf("the replacement answered %d; want 200", fr.Status)
	}
	if c, gaps := st.told(); len(c) != 1 || len(gaps) != 1 || !gaps[0].Initial {
		t.Errorf("OnConnection was told %v and OnSocketGap %v; want up once and the initial gap alone", c, gaps)
	}
}

// A SocketClosing ends the socket: with reconnect, the region connects again at once and counts the code; without, it waits.
func TestSocketModeTakesEachSocketClosingCode(t *testing.T) {
	tests := []struct {
		code, reason string
		reconnect    bool
	}{
		{"SocketModeDisabled", "socket_mode_disabled", true},
		{"Replaced", "replaced", true},
		{"TokenReused", "token_reused", true},
		{"NoIdentity", "no_identity", true},
		{"SomethingNew", "closing_other", true},
		{"SocketModeDisabled", "socket_mode_disabled", false},
	}
	for _, tc := range tests {
		t.Run(tc.code+map[bool]string{true: "/reconnect", false: "/wait"}[tc.reconnect], func(t *testing.T) {
			st := newSocketTest(t)
			st.c.sockets.reconnectNow = 0 // a socket of any age is followed at once
			st.run(t)
			sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
			st.up(t, true)
			sock.Closing(t, tc.code, tc.reconnect)
			st.up(t, false)
			if !tc.reconnect {
				time.Sleep(200 * time.Millisecond)
				if n := len(st.f.RegionConnects("amer")); n != 1 {
					t.Errorf("%d connect calls after a SocketClosing that says not to reconnect; want none past the first", n)
				}
				return
			}
			st.f.WaitSocket(t, "amer", 1, 5*time.Second)
			st.up(t, true)
			if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "amer", tc.reason); got != 1 {
				t.Errorf("reconnects{amer,%s} = %v; want 1", tc.reason, got)
			}
			select {
			case <-sock.Closed():
			case <-time.After(5 * time.Second):
				t.Error("the closing socket was not closed")
			}
		})
	}
}

// A connect call answered 401 ends Run with an error that Classify says is FatalAuth: the bot's token is refused.
func TestSocketModeStopsWhenTheConnectCallRefusesTheToken(t *testing.T) {
	st := newSocketTest(t)
	st.f.Answer("amer", faketeams.Answer{Status: http.StatusUnauthorized, Body: `{"error":"Unauthorized"}`})
	st.run(t)
	err := st.ended(t)
	var se *SocketError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("Run = %v; want the connect call's 401", err)
	}
	if v := st.c.Classify(chat.OpOther, err); v.Class != chat.FatalAuth {
		t.Errorf("Classify(401) = %+v; want FatalAuth", v)
	}
	if n := len(st.f.Connects()); n != 1 {
		t.Errorf("%d connect calls; want 1: a refused token is not tried again", n)
	}
}

// Every region answering 403 ends Run with the last refusal, which Classify says may pass (the broker runs it again on its backoff);
// the error carries Microsoft's message.
func TestSocketModeStopsWhenEveryRegionRefuses(t *testing.T) {
	st := newSocketTest(t, "amer", "apac")
	for _, r := range []string{"amer", "apac"} {
		st.f.Answer(r, faketeams.Answer{Status: http.StatusForbidden, Body: `{"error":"Socket mode is not enabled for this bot."}`})
	}
	st.run(t)
	err := st.ended(t)
	var se *SocketError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || !strings.Contains(err.Error(), "Socket mode is not enabled for this bot.") {
		t.Fatalf("Run = %v; want a 403 with Microsoft's message", err)
	}
	if v := st.c.Classify(chat.OpOther, err); v.Class != chat.Transient {
		t.Errorf("Classify(every region 403) = %+v; want Transient", v)
	}
}

// A region that answers 403 while another is ready is logged and tried again on the slow wait, and the connection stays up; once it
// accepts, it has a socket too.
func TestSocketModeKeepsTheRegionsThatAnswer(t *testing.T) {
	st := newSocketTest(t, "amer", "apac")
	st.c.sockets.refusedRetry = 300 * time.Millisecond
	st.f.Answer("apac", faketeams.Answer{Status: http.StatusForbidden, Body: `{"error":"Socket mode is not enabled for this bot."}`})
	st.run(t)
	st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	st.up(t, true)
	connects := st.f.WaitConnects(t, "apac", 2, 5*time.Second)
	if d := connects[1].At.Sub(connects[0].At); d < 200*time.Millisecond {
		t.Errorf("apac was tried again %s after its 403; want the slow wait", d)
	}
	if !strings.Contains(st.logs.String(), "a region refused the bot's socket") {
		t.Error("the refused region was not logged")
	}
	st.f.Answer("apac", faketeams.Answer{})
	st.f.WaitSocket(t, "apac", 0, 5*time.Second)
	testutil.Eventually(t, "apac's socket to be ready", func() bool { return st.series(t, "agentkourier_teams_socket_ready", "apac") == 1 })
	if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "apac", "refused"); got < 1 {
		t.Errorf("reconnects{apac,refused} = %v; want at least 1", got)
	}
	select {
	case err := <-st.done:
		t.Fatalf("Run ended (%v) while a region was ready", err)
	default:
	}
}

// A 429 waits its Retry-After, which is longer than the backoff; a 503 is tried again on the backoff. Each counts its reason.
func TestSocketModeWaitsOnA429AndRetriesA503(t *testing.T) {
	tests := []struct {
		name    string
		answer  faketeams.Answer
		reason  string
		atLeast time.Duration
	}{
		{"429", faketeams.Answer{Status: http.StatusTooManyRequests, RetryAfter: "1", Body: `{"error":"The bot already has 2 socket connections open."}`}, "rate_limited", 900 * time.Millisecond},
		{"503", faketeams.Answer{Status: http.StatusServiceUnavailable}, "unavailable", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newSocketTest(t)
			st.f.Answer("amer", tc.answer)
			st.run(t)
			st.f.WaitConnects(t, "amer", 1, 5*time.Second)
			st.f.Answer("amer", faketeams.Answer{})
			st.f.WaitSocket(t, "amer", 0, 5*time.Second)
			st.up(t, true)
			connects := st.f.RegionConnects("amer")
			if d := connects[1].At.Sub(connects[0].At); d < tc.atLeast {
				t.Errorf("the second connect came %s after the first; want at least %s", d, tc.atLeast)
			}
			if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "amer", tc.reason); got < 1 {
				t.Errorf("reconnects{amer,%s} = %v; want at least 1", tc.reason, got)
			}
		})
	}
}

// The run's end, as when the leader stops leading, closes the socket with a close frame, and says the connection is down.
func TestSocketModeClosesTheSocketWhenRunEnds(t *testing.T) {
	st := newSocketTest(t)
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	st.up(t, true)
	if err := st.stop(t); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want context.Canceled", err)
	}
	select { // the fake reads the close at its own pace
	case <-sock.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the socket was still open after Run returned")
	}
	if !sock.ClosedByClient() {
		t.Error("the socket was not closed by the client's close frame")
	}
	if c, _ := st.told(); c[len(c)-1] {
		t.Error("OnConnection was last told up after Run returned")
	}
	if got := st.series(t, "agentkourier_teams_socket_ready", "amer"); got != 0 {
		t.Errorf("agentkourier_teams_socket_ready{amer} = %v after Run; want 0", got)
	}
	if n := len(st.f.Connects()); n != 1 {
		t.Errorf("%d connect calls; want 1, none after the stop", n)
	}
}

// A socket that drops with none other ready opens a gap: the connection is down, a WARN line says since when, and when a socket is
// ready again the gap, with its start and end, is logged, observed and given to OnSocketGap. The start of the run is a gap too, marked
// Initial. No token is ever logged.
func TestSocketModeRecordsEachGap(t *testing.T) {
	st := newSocketTest(t)
	st.c.sockets.reconnectNow = 0
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	st.up(t, true)
	dropped := time.Now()
	sock.Drop()
	st.f.WaitSocket(t, "amer", 1, 5*time.Second)
	testutil.Eventually(t, "the gap to be told", func() bool { _, g := st.told(); return len(g) == 2 })
	c, gaps := st.told()
	if len(c) != 3 || !c[0] || c[1] || !c[2] {
		t.Errorf("OnConnection was told %v; want up, down, up", c)
	}
	if !gaps[0].Initial || gaps[1].Initial {
		t.Errorf("gaps = %+v; want the initial one, then one that is not", gaps)
	}
	if g := gaps[1]; g.Start.Before(dropped) || !g.End.After(g.Start) {
		t.Errorf("the gap = %+v; want it from the drop (%s) to the next socket", g, dropped)
	}
	if got := st.series(t, "agentkourier_teams_socket_gap_seconds"); got != 2 {
		t.Errorf("agentkourier_teams_socket_gap_seconds has %v observations; want 2", got)
	}
	if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "amer", "closed"); got != 1 {
		t.Errorf("reconnects{amer,closed} = %v; want 1", got)
	}
	logs := st.logs.String()
	for _, want := range []string{"no socket of the connection is ready", "a socket is ready again; what Teams sent while none was is lost"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %q", want)
		}
	}
	for _, tok := range []string{"fake-token-", faketeams.ConnectToken, faketeams.ServiceToken, "fake-connection-token", testClientSecret} {
		if strings.Contains(logs, tok) {
			t.Errorf("the log holds the token %q", tok)
		}
	}
}

// A socket on which the service never calls SocketReady is closed at readyWait, and its region connects again; one on which the service
// sends nothing for serverTimeout is closed then. The client pings meanwhile.
func TestSocketModeClosesASilentSocket(t *testing.T) {
	t.Run("no SocketReady", func(t *testing.T) {
		st := newSocketTest(t)
		st.c.sockets.readyWait = 200 * time.Millisecond
		st.f.Silence("amer", true)
		st.run(t)
		first := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
		st.f.WaitConnects(t, "amer", 2, 5*time.Second)
		<-first.Closed()
		st.f.Silence("amer", false)
		testutil.Eventually(t, "a ready socket", func() bool { c, _ := st.told(); return len(c) > 0 })
		if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "amer", "ready_timeout"); got < 1 {
			t.Errorf("reconnects{amer,ready_timeout} = %v; want at least 1", got)
		}
	})
	t.Run("no message for the server timeout", func(t *testing.T) {
		st := newSocketTest(t)
		st.c.sockets.serverTimeout, st.c.sockets.pingEvery, st.c.sockets.reconnectNow = 400*time.Millisecond, 50*time.Millisecond, 0
		st.run(t)
		sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
		st.f.WaitSocket(t, "amer", 1, 5*time.Second)
		<-sock.Closed()
		if sock.Pings() == 0 {
			t.Error("the client sent no ping")
		}
		if got := st.series(t, "agentkourier_teams_socket_reconnects_total", "amer", "server_timeout"); got != 1 {
			t.Errorf("reconnects{amer,server_timeout} = %v; want 1", got)
		}
	})
}

// A socket that ends soon after it was ready is followed after the backoff, not at once, so a service that closes each socket as it
// opens does not make a busy loop.
func TestSocketModeBacksOffASocketThatEndsAtOnce(t *testing.T) {
	st := newSocketTest(t)
	st.c.sockets.retryBase, st.c.sockets.retryMax = 300*time.Millisecond, 300*time.Millisecond
	st.run(t)
	sock := st.f.WaitSocket(t, "amer", 0, 5*time.Second)
	sock.Drop()
	connects := st.f.WaitConnects(t, "amer", 2, 5*time.Second)
	if d := connects[1].At.Sub(connects[0].At); d < 250*time.Millisecond {
		t.Errorf("the next connect came %s after a socket that lived a moment; want the backoff", d)
	}
}

func TestNegotiateURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://h/client/?hub=bothub", "https://h/client/negotiate?hub=bothub&negotiateVersion=1"},
		{"https://h/client", "https://h/client/negotiate?negotiateVersion=1"},
		{"https://h/client/?hub=bothub&asrs.op=%2Fclient", "https://h/client/negotiate?hub=bothub&asrs.op=%2Fclient&negotiateVersion=1"},
	}
	for _, tc := range tests {
		if got := negotiateURL(tc.in); got != tc.want {
			t.Errorf("negotiateURL(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// A connection in http mode has no sockets, and offers its route's handler.
func TestHTTPModeHasNoSockets(t *testing.T) {
	ct := newClientTest(t)
	if ct.c.sockets != nil {
		t.Error("a connection in http mode has sockets")
	}
	if h, ok := ct.c.HTTPHandler(); !ok || h == nil {
		t.Error("a connection in http mode offers no handler")
	}
}
