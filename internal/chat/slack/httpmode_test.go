package slack_test

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// signingSecret is the fake signing secret of the app in HTTP mode.
const signingSecret = "fake-signing-secret"

// stubSlack is a Web API that answers auth.test for the fake bot and records every method called, so that a test can see that nothing
// on the path of a request calls Slack.
type stubSlack struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []string
	authErr string // auth.test answers ok:false with this code, if set
}

func newStubSlack(t *testing.T) *stubSlack {
	t.Helper()
	before := goleak.IgnoreCurrent()
	s := &stubSlack{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		s.mu.Lock()
		s.calls = append(s.calls, method)
		authErr := s.authErr
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if method == "auth.test" && authErr == "" {
			_, _ = io.WriteString(w, `{"ok":true,"url":"https://fake.slack.com/","team_id":"TFAKE","user_id":"UFAKEBOT","bot_id":"BFAKEBOT"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, cmp.Or(authErr, "unknown_method"))
	}))
	t.Cleanup(func() {
		s.Close()
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	return s
}

// methods is every Web API method called so far.
func (s *stubSlack) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// httpConfig is a Client's config in HTTP mode against s.
func httpConfig(s *stubSlack) slack.Config {
	return slack.Config{BotToken: secret.New(botToken), SigningSecret: secret.New(signingSecret), APIURL: s.URL + "/", Connection: intakeConn,
		Logger: testutil.DiscardLogger()}
}

// httpClient is a Client in HTTP mode against s, its handler, and a channel that OnConnection reports to.
func httpClient(t *testing.T, s *stubSlack) (*slack.Client, http.Handler, <-chan bool) {
	t.Helper()
	conn := make(chan bool, 16)
	cfg := httpConfig(s)
	cfg.OnConnection = func(up bool) { conn <- up }
	c := slack.New(cfg)
	h, ok := c.HTTPHandler()
	if !ok || h == nil {
		t.Fatal("HTTPHandler in HTTP mode = none; want the handler")
	}
	return c, h, conn
}

// runHTTP runs c under parent until the test ends, once it takes requests, and returns the func that ends Run and waits for it.
func runHTTP(parent context.Context, t *testing.T, c *slack.Client, conn <-chan bool, sk slack.Sink) (stop func() error) {
	t.Helper()
	cancel, wait := runIn(parent, t, c, sk)
	select {
	case up := <-conn:
		if !up {
			t.Fatal("OnConnection said down first; want up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not start taking requests")
	}
	return func() error { cancel(); return wait() }
}

// signed is the headers of a request whose body is body, signed with secret at the time at, as Slack signs one.
func signed(body string, at time.Time, secret string) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", ts)
	h.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

// eventBody is the Events API payload of event.
func eventBody(event map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "TFAKE", "event_id": fmt.Sprint("Ev", event["ts"]), "event": event})
	return string(b)
}

// pressBody is the form of a press, by user, of a button on the message at ts in channel.
func pressBody(user, channel, ts string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user},
		"container": map[string]any{"type": "message", "channel_id": channel, "message_ts": ts},
		"actions":   []any{map[string]any{"type": "button", "action_id": "submit", "block_id": "b1", "value": "v1"}},
	})
	return url.Values{"payload": {string(b)}}.Encode()
}

const (
	jsonType = "application/json"
	formType = "application/x-www-form-urlencoded"
)

// deliver posts body to h with the headers hdr, under ctx, and returns the answer.
func deliverTo(ctx context.Context, h http.Handler, contentType, body string, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/chat/slack/agent-kourier-system/slack-main", strings.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// post is deliverTo, signed now with the app's secret.
func post(t *testing.T, h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	return deliverTo(t.Context(), h, contentType, body, signed(body, time.Now(), signingSecret))
}

// A request that the app did not sign, with the signing secret over its timestamp and its body, is refused 401 before anything of it is
// decoded or handed on, and so is one whose timestamp is more than five minutes from the clock, either way, which a replay is. A body
// that is no JSON is refused as unsigned, not as malformed: it was never read.
func TestHTTPModeRefusesARequestTheAppDidNotSign(t *testing.T) {
	h := telemetrytest.New(t)
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	sk := &sink{}
	runHTTP(h.Ctx, t, c, conn, sk)
	body := eventBody(message(nil))
	now := time.Now()
	for _, tc := range []struct {
		name, body, outcome string
		hdr                 http.Header
	}{
		{"no headers", body, "bad_signature", http.Header{}},
		{"another secret", body, "bad_signature", signed(body, now, "another-secret")},
		{"the signature of another body", body, "bad_signature", signed(eventBody(message(map[string]any{"text": "other"})), now, signingSecret)},
		{"a signature that is not hex", body, "bad_signature", func() http.Header {
			hdr := signed(body, now, signingSecret)
			hdr.Set("X-Slack-Signature", "v0=zz")
			return hdr
		}()},
		{"a signature of another version", body, "bad_signature", func() http.Header {
			hdr := signed(body, now, signingSecret)
			hdr.Set("X-Slack-Signature", "v1="+strings.TrimPrefix(hdr.Get("X-Slack-Signature"), "v0="))
			return hdr
		}()},
		{"a timestamp that is not a number", body, "bad_signature", func() http.Header {
			hdr := signed(body, now, signingSecret)
			hdr.Set("X-Slack-Request-Timestamp", "soon")
			return hdr
		}()},
		{"signed six minutes ago", body, "stale", signed(body, now.Add(-6*time.Minute), signingSecret)},
		{"signed six minutes ahead", body, "stale", signed(body, now.Add(6*time.Minute), signingSecret)},
		{"no JSON, and unsigned", "{not json", "bad_signature", http.Header{}},
	} {
		rec := deliverTo(t.Context(), handler, jsonType, tc.body, tc.hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: answered %d; want 401", tc.name, rec.Code)
		}
	}
	if got := sk.messages(); len(got) != 0 {
		t.Errorf("the sink was given %+v; want nothing of a request that is not signed", got)
	}
	for outcome, want := range map[string]float64{"bad_signature": 7, "stale": 2} {
		if got := h.Counter(t, "agentkourier_slack_http_requests_total", map[string]string{"outcome": outcome, "connection": intakeConn}); got != want {
			t.Errorf("requests %s = %v; want %v", outcome, got, want)
		}
	}
	// Signed four minutes ago, it is within the age and taken.
	if rec := deliverTo(t.Context(), handler, jsonType, body, signed(body, now.Add(-4*time.Minute), signingSecret)); rec.Code != http.StatusOK {
		t.Errorf("a request signed four minutes ago: answered %d; want 200", rec.Code)
	}
}

// A url_verification, signed, is answered with its challenge, whether or not a Run takes requests: Slack sends it when the Request URL is
// set, which may be before the connection runs.
func TestHTTPModeAnswersAURLVerificationWithItsChallenge(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	body := `{"token":"legacy","challenge":"fake-challenge-3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`
	check := func(when string) {
		rec := post(t, handler, jsonType, body)
		var got struct{ Challenge string }
		if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Challenge != "fake-challenge-3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
			t.Errorf("%s: answered %d %q; want 200 and the challenge", when, rec.Code, rec.Body.String())
		}
	}
	check("before Run")
	runHTTP(t.Context(), t, c, conn, &sink{})
	check("while Run takes requests")
}

// A message event is handed to the sink, decoded as in Socket Mode, and answered 200 once the sink took it. The answer comes in well
// under the time the handler may wait, since nothing on the path of a request calls Slack or an agent: the only call to Slack is Run's
// auth.test, and Run opens no Socket Mode connection.
func TestHTTPModeHandsAMessageToTheSinkAndAnswersAtOnce(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	sk := &sink{}
	runHTTP(t.Context(), t, c, conn, sk)
	started := time.Now()
	rec := post(t, handler, jsonType, eventBody(message(map[string]any{"channel": "C9", "ts": "7.7", "event_ts": "7.7", "text": "is it down?"})))
	if took := time.Since(started); took > slack.HTTPAckWait/2 {
		t.Errorf("the answer took %v; want it as soon as the sink returned, well under %v", took, slack.HTTPAckWait)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d %q; want 200", rec.Code, rec.Body.String())
	}
	if got := sk.messages(); len(got) != 1 || got[0].Channel != "C9" || got[0].TS != "7.7" || got[0].Text != "is it down?" || got[0].User != "U1" {
		t.Errorf("the sink was given %+v; want the message", got)
	}
	if got := stub.methods(); !slices.Equal(got, []string{"auth.test"}) {
		t.Errorf("Web API methods called = %v; want auth.test alone", got)
	}
}

// What is no turn is answered 200 and not handed on, as Socket Mode acknowledges it: a bot's message that no trigger names, Agent
// Kourier's own post, an edit, and an Events API payload that is no event (app_rate_limited).
func TestHTTPModeAcknowledgesWhatIsNoTurnWithoutTheSink(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	sk := &sink{}
	runHTTP(t.Context(), t, c, conn, sk)
	for name, body := range map[string]string{
		"a bot's message":      eventBody(message(map[string]any{"bot_id": "BOTHER", "user": "", "subtype": "bot_message"})),
		"Agent Kourier's post": eventBody(message(map[string]any{"bot_id": "BFAKEBOT", "user": "UFAKEBOT"})),
		"an edit":              eventBody(message(map[string]any{"subtype": "message_changed"})),
		"app_rate_limited":     `{"token":"legacy","type":"app_rate_limited","team_id":"TFAKE","minute_rate_limited":1518467820,"api_app_id":"AFAKE"}`,
	} {
		if rec := post(t, handler, jsonType, body); rec.Code != http.StatusOK {
			t.Errorf("%s: answered %d; want 200", name, rec.Code)
		}
	}
	if got := sk.messages(); len(got) != 0 {
		t.Errorf("the sink was given %+v; want none", got)
	}
}

// A press arrives form-encoded, its JSON in the field payload, and is handed to the sink as Socket Mode hands one.
func TestHTTPModeHandsAPressToTheSink(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	sk := &sink{}
	runHTTP(t.Context(), t, c, conn, sk)
	if rec := post(t, handler, formType, pressBody("U2", "C1", "5.5")); rec.Code != http.StatusOK {
		t.Fatalf("answered %d %q; want 200", rec.Code, rec.Body.String())
	}
	got := sk.interactions()
	if len(got) != 1 || got[0].User != "U2" || got[0].Channel != "C1" || got[0].MessageTS != "5.5" || got[0].ActionID != "submit" || got[0].Value != "v1" {
		t.Errorf("the sink was given %+v; want the press", got)
	}
	// Agent Kourier's own press is no person's, and is acknowledged without the sink.
	if rec := post(t, handler, formType, pressBody("UFAKEBOT", "C1", "5.5")); rec.Code != http.StatusOK || len(sk.interactions()) != 1 {
		t.Errorf("the bot's own press: answered %d, sink has %d presses; want 200 and the sink not called", rec.Code, len(sk.interactions()))
	}
}

// A signed request that is no event and no press the adapter can read is answered 400.
func TestHTTPModeAnswers400ToASignedRequestItCannotRead(t *testing.T) {
	h := telemetrytest.New(t)
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	runHTTP(h.Ctx, t, c, conn, &sink{})
	for _, tc := range []struct{ name, contentType, body string }{
		{"no JSON", jsonType, "{not json"},
		{"a form with no payload", formType, "command=%2Fdeploy&text=now"},
		{"a payload that is no JSON", formType, url.Values{"payload": {"{not json"}}.Encode()},
		{"a url_verification with no challenge", jsonType, `{"type":"url_verification"}`},
	} {
		if rec := post(t, handler, tc.contentType, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: answered %d; want 400", tc.name, rec.Code)
		}
	}
	if got := h.Counter(t, "agentkourier_slack_http_requests_total", map[string]string{"outcome": "malformed", "connection": intakeConn}); got != 4 {
		t.Errorf("requests malformed = %v; want 4", got)
	}
}

// An event or a press the sink did not take is answered 503, for Slack to send again.
func TestHTTPModeWithholdsTheAnswerWhenTheSinkFails(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	fail := errors.New("fake: not taken")
	sk := &sink{fn: func(slack.Message) error { return fail }, ifn: func(slack.Interaction) error { return fail }}
	runHTTP(t.Context(), t, c, conn, sk)
	if rec := post(t, handler, jsonType, eventBody(message(nil))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a message the sink refused: answered %d; want 503", rec.Code)
	}
	if rec := post(t, handler, formType, pressBody("U2", "C1", "5.5")); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a press the sink refused: answered %d; want 503", rec.Code)
	}
}

// When no Run takes requests, before the first and after the last, a signed event is answered 503 at once and the sink is not called: the
// handler may be called after its run has ended, and neither blocks nor hands the event to a sink that is gone.
func TestHTTPModeAnswers503WhenNoRunTakesRequests(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	c.SetHTTPAckWait(time.Minute) // were the request handed to a lane, the test would wait for it
	sk := &sink{}
	check := func(when string) {
		started := time.Now()
		if rec := post(t, handler, jsonType, eventBody(message(nil))); rec.Code != http.StatusServiceUnavailable || time.Since(started) > time.Second {
			t.Errorf("%s: answered %d after %v; want 503 at once", when, rec.Code, time.Since(started))
		}
	}
	check("before Run")
	stop := runHTTP(t.Context(), t, c, conn, sk)
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Run after cancel = %v; want context.Canceled", err)
	}
	check("after Run")
	if got := sk.messages(); len(got) != 0 {
		t.Errorf("the sink was given %+v; want nothing while no Run takes requests", got)
	}
}

// A request whose sink is still working when Run ends is answered 503 then, without waiting out the handler's bound, and Run returns once
// the sink has.
func TestHTTPModeAnswersARequestInFlightWhenRunEnds(t *testing.T) {
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	c.SetHTTPAckWait(time.Minute)
	held, release := make(chan struct{}), make(chan struct{})
	sk := &sink{fn: func(slack.Message) error { close(held); <-release; return nil }}
	stop := runHTTP(t.Context(), t, c, conn, sk)
	answered := make(chan int, 1)
	go func() { answered <- post(t, handler, jsonType, eventBody(message(nil))).Code }()
	<-held
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	select {
	case code := <-answered:
		if code != http.StatusServiceUnavailable {
			t.Errorf("answered %d when Run ended; want 503", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not answered when Run ended")
	}
	select {
	case err := <-stopped:
		t.Errorf("Run returned (%v) while the sink was still working; want it to wait for the sink", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want context.Canceled", err)
	}
}

// A sink that has not answered within the handler's bound, or by the deadline of the request, gets the request answered 503, for Slack
// to send again; the sink's work is not cut.
func TestHTTPModeAnswers503WhenTheSinkIsLate(t *testing.T) {
	h := telemetrytest.New(t)
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	c.SetHTTPAckWait(50 * time.Millisecond)
	release := make(chan struct{})
	sk := &sink{fn: func(slack.Message) error { <-release; return nil }}
	runHTTP(h.Ctx, t, c, conn, sk)
	t.Cleanup(func() { close(release) })
	if rec := post(t, handler, jsonType, eventBody(message(map[string]any{"channel": "C1"}))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("past the handler's bound: answered %d; want 503", rec.Code)
	}
	c.SetHTTPAckWait(time.Minute)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	body := eventBody(message(map[string]any{"channel": "C2"}))
	if rec := deliverTo(ctx, handler, jsonType, body, signed(body, time.Now(), signingSecret)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("past the request's deadline: answered %d; want 503", rec.Code)
	}
	if got := h.Counter(t, "agentkourier_slack_http_requests_total", map[string]string{"outcome": "timeout", "connection": intakeConn}); got != 2 {
		t.Errorf("requests timeout = %v; want 2", got)
	}
}

// Past the events a channel's lane holds, a request is answered 503 at once and counted as shed, as an envelope is left unacknowledged in
// Socket Mode; the requests the lane holds are answered 200 once the sink takes them.
func TestHTTPModeShedsAnEventPastTheChannelBacklog(t *testing.T) {
	h := telemetrytest.New(t)
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	c.SetHTTPAckWait(time.Minute)
	release := make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	sk := &sink{fn: func(slack.Message) error { <-release; return nil }}
	runHTTP(h.Ctx, t, c, conn, sk)
	t.Cleanup(open)
	codes := make(chan int, slack.MaxChannelBacklog)
	for i := range slack.MaxChannelBacklog {
		body := eventBody(message(map[string]any{"ts": fmt.Sprintf("%d.1", i+1), "event_ts": fmt.Sprintf("%d.1", i+1)}))
		go func() { codes <- post(t, handler, jsonType, body).Code }()
	}
	testutil.Eventually(t, "the lane of C1 did not fill", func() bool { return c.PendingOn("C1") == slack.MaxChannelBacklog }, testutil.Within(5*time.Second))
	if rec := post(t, handler, jsonType, eventBody(message(map[string]any{"ts": "99.1", "event_ts": "99.1"}))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("past the backlog: answered %d; want 503", rec.Code)
	}
	open()
	for range slack.MaxChannelBacklog {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("a request the lane held: answered %d; want 200", code)
		}
	}
	for name, want := range map[string]map[string]string{
		"agentkourier_slack_http_requests_total": {"outcome": "shed", "connection": intakeConn},
		"agentkourier_slack_events_shed_total":   {"reason": "channel_backlog", "connection": intakeConn},
	} {
		if got := h.Counter(t, name, want); got != 1 {
			t.Errorf("%s%v = %v; want 1", name, want, got)
		}
	}
}

// Each answer is counted under its outcome and the connection, a signed request that Slack marks as a retry is counted as one, and the
// time to the answer of an event handed to the sink is observed as in Socket Mode.
func TestHTTPModeCountsRequestsAndRetriesByConnection(t *testing.T) {
	h := telemetrytest.New(t)
	stub := newStubSlack(t)
	c, handler, conn := httpClient(t, stub)
	runHTTP(h.Ctx, t, c, conn, &sink{})
	body := eventBody(message(nil))
	retry := signed(body, time.Now(), signingSecret)
	retry.Set("X-Slack-Retry-Num", "1")
	retry.Set("X-Slack-Retry-Reason", "http_timeout")
	for _, hdr := range []http.Header{signed(body, time.Now(), signingSecret), retry} {
		if rec := deliverTo(t.Context(), handler, jsonType, body, hdr); rec.Code != http.StatusOK {
			t.Fatalf("answered %d; want 200", rec.Code)
		}
	}
	unsigned := http.Header{"X-Slack-Retry-Num": {"2"}} // not Slack's: its header counts for nothing
	deliverTo(t.Context(), handler, jsonType, body, unsigned)
	for name, tc := range map[string]struct {
		labels map[string]string
		want   float64
	}{
		"agentkourier_slack_http_requests_total": {map[string]string{"outcome": "acknowledged", "connection": intakeConn}, 2},
		"agentkourier_slack_http_retries_total":  {map[string]string{"connection": intakeConn}, 1},
	} {
		if got := h.Counter(t, name, tc.labels); got != tc.want {
			t.Errorf("%s%v = %v; want %v", name, tc.labels, got, tc.want)
		}
	}
	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var observed uint64
	for _, f := range families {
		if f.GetName() != "agentkourier_slack_ack_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == "acknowledged" {
					observed += m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	if observed != 2 {
		t.Errorf("acknowledgments observed = %d; want the two events", observed)
	}
}

// In HTTP mode the transport is up from when auth.test has passed until Run ends, and never when auth.test fails: OnConnection says so,
// which the broker shows as agentkourier_chat_connected.
func TestHTTPModeIsConnectedWhileRunTakesRequests(t *testing.T) {
	stub := newStubSlack(t)
	c, _, conn := httpClient(t, stub)
	stop := runHTTP(t.Context(), t, c, conn, &sink{}) // it waits for the first report, which must be up
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Run after cancel = %v; want context.Canceled", err)
	}
	if up := <-conn; up {
		t.Error("OnConnection said up when Run ended; want down")
	}

	stub.mu.Lock()
	stub.authErr = "invalid_auth"
	stub.mu.Unlock()
	c2, handler2, conn2 := httpClient(t, stub)
	if err := c2.Run(t.Context(), &sink{}); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("Run with a rejected token = %v; want the rejection", err)
	}
	select {
	case up := <-conn2:
		t.Errorf("OnConnection said %v when auth.test failed; want nothing", up)
	default:
	}
	if rec := post(t, handler2, jsonType, eventBody(message(nil))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a request to a client whose Run failed: answered %d; want 503", rec.Code)
	}
}

// A client in Socket Mode takes no requests over HTTP, and neither does its platform; a client in HTTP mode gives its platform its handler.
func TestOnlyAClientInHTTPModeTakesRequests(t *testing.T) {
	stub := newStubSlack(t)
	socket := slack.New(slack.Config{BotToken: secret.New(botToken), AppToken: secret.New(appToken), APIURL: stub.URL + "/"})
	if h, ok := socket.HTTPHandler(); ok || h != nil {
		t.Errorf("Client.HTTPHandler in Socket Mode = %v, %v; want none", h, ok)
	}
	if _, ok := slack.NewPlatform(socket, intakeConn).HTTPHandler(); ok {
		t.Error("Platform.HTTPHandler in Socket Mode says it takes requests; want none")
	}
	var p chat.Platform = slack.NewPlatform(slack.New(httpConfig(stub)), intakeConn)
	r, ok := p.(chat.HTTPReceiver)
	if !ok {
		t.Fatal("the Slack platform is no chat.HTTPReceiver")
	}
	if h, ok := r.HTTPHandler(); !ok || h == nil {
		t.Error("Platform.HTTPHandler in HTTP mode = none; want the client's handler")
	}
}

// The platform's handler hands what it decodes to the neutral sink that Platform.Run was given, as the neutral model has it.
func TestThePlatformsHandlerReachesTheNeutralSink(t *testing.T) {
	stub := newStubSlack(t)
	conn := make(chan bool, 4)
	cfg := httpConfig(stub)
	cfg.OnConnection = func(up bool) { conn <- up }
	p := slack.NewPlatform(slack.New(cfg), intakeConn)
	handler, _ := p.HTTPHandler()
	fake := &neutralSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, fake) }()
	t.Cleanup(func() { cancel(); _ = <-done })
	if up := <-conn; !up {
		t.Fatal("OnConnection said down first")
	}
	if rec := post(t, handler, jsonType, eventBody(message(map[string]any{"channel": "C3", "ts": "3.3"}))); rec.Code != http.StatusOK {
		t.Fatalf("answered %d; want 200", rec.Code)
	}
	if got := fake.got(); len(got) != 1 || got[0].At.Conn != intakeConn || got[0].At.Channel != "C3" || got[0].At.ID != "3.3" {
		t.Errorf("the neutral sink was given %+v; want the message on the connection", got)
	}
}

// neutralSink records the neutral messages it is given.
type neutralSink struct {
	mu   sync.Mutex
	msgs []chat.Inbound
}

func (s *neutralSink) Message(_ context.Context, m chat.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *neutralSink) Interaction(context.Context, chat.Interaction) error { return nil }

func (s *neutralSink) got() []chat.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}
