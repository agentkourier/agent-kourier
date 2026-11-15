package fakeslack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// testSigningSecret is the fake signing secret of the apps in HTTP mode. It is a fake constant.
const testSigningSecret = "fake-signing-secret"

// received is one request the fake sent to an app's request URL, as the receiver read it.
type received struct {
	header http.Header
	body   []byte
}

// receiver is an app's request URL: it records every request, and answers each with what answer says (200 when answer is nil).
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	got    []received
	answer func(n int, r received, w http.ResponseWriter) // n counts the requests from 0
	more   chan struct{}                                  // a value for each request recorded
}

func newReceiver(t *testing.T, answer func(n int, r received, w http.ResponseWriter)) *receiver {
	t.Helper()
	rc := &receiver{answer: answer, more: make(chan struct{}, 1000)}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("receiver: read: %v", err)
		}
		got := received{header: r.Header.Clone(), body: body}
		rc.mu.Lock()
		n := len(rc.got)
		rc.got = append(rc.got, got)
		rc.mu.Unlock()
		rc.more <- struct{}{}
		if rc.answer != nil {
			rc.answer(n, got, w)
		}
	}))
	t.Cleanup(rc.Close)
	return rc
}

// wait returns the first n requests once they have come, and fails the test if they do not within 10 seconds.
func (rc *receiver) wait(t *testing.T, n int) []received {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		rc.mu.Lock()
		if len(rc.got) >= n {
			out := append([]received(nil), rc.got[:n]...)
			rc.mu.Unlock()
			return out
		}
		rc.mu.Unlock()
		select {
		case <-rc.more:
		case <-timeout:
			t.Fatalf("the receiver got fewer than %d requests within 10s", n)
		}
	}
}

// count is how many requests the receiver has had.
func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

// httpFake starts a fake whose default app delivers over HTTP to url, with the events for the bot's own posts off, so that a test sees
// only what it sends, and a short retry schedule.
func httpFake(t *testing.T, url string, opts ...fakeslack.Option) *fakeslack.Server {
	t.Helper()
	leakCheck(t)
	opts = append([]fakeslack.Option{
		fakeslack.WithRequestURL(url, testSigningSecret), fakeslack.WithoutOwnPostEvents(),
		fakeslack.WithHTTPRetries(10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond),
	}, opts...)
	fake := fakeslack.New(opts...)
	t.Cleanup(fake.Close)
	return fake
}

// verifySigned fails the test unless r is signed as Slack signs: a fresh timestamp, and a signature that slack-go's verifier, which
// the broker uses, accepts with the signing secret.
func verifySigned(t *testing.T, r received) {
	t.Helper()
	ts, err := strconv.ParseInt(r.header.Get("X-Slack-Request-Timestamp"), 10, 64)
	if err != nil {
		t.Fatalf("X-Slack-Request-Timestamp = %q: %v", r.header.Get("X-Slack-Request-Timestamp"), err)
	}
	if age := time.Since(time.Unix(ts, 0)); age < -time.Minute || age > time.Minute {
		t.Errorf("the request's timestamp is %v from now; want the time it was sent", age)
	}
	sv, err := slack.NewSecretsVerifier(r.header, testSigningSecret)
	if err != nil {
		t.Fatalf("NewSecretsVerifier: %v", err)
	}
	if _, err := sv.Write(r.body); err != nil {
		t.Fatal(err)
	}
	if err := sv.Ensure(); err != nil {
		t.Errorf("the signature does not verify with the signing secret: %v", err)
	}
	if !strings.HasPrefix(r.header.Get("X-Slack-Signature"), "v0=") {
		t.Errorf("X-Slack-Signature = %q; want v0= and the hex of the HMAC", r.header.Get("X-Slack-Signature"))
	}
}

// callback decodes an Events API request.
func callback(t *testing.T, r received) slackevents.EventsAPIEvent {
	t.Helper()
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json for an event", ct)
	}
	ev, err := slackevents.ParseEvent(r.body, slackevents.OptionNoVerifyToken())
	if err != nil {
		t.Fatalf("the body is no Events API payload: %v\n%s", err, r.body)
	}
	return ev
}

func TestHTTPDeliverySignsEventsAsSlackDoesAndReadsTheAckFromThe200(t *testing.T) {
	rc := newReceiver(t, nil)
	fake := httpFake(t, rc.URL)

	m, err := fake.Mention("C1", "U1", "<@UFAKEBOT> hello")
	if err != nil || len(m.Envelopes) != 2 {
		t.Fatalf("Mention = %+v, %v; want two deliveries", m, err)
	}
	waitAcked(t, fake, m.Envelopes...)
	got := rc.wait(t, 2)
	for i, want := range []string{"app_mention", "message"} {
		verifySigned(t, got[i])
		ev := callback(t, got[i])
		if ev.Type != slackevents.CallbackEvent || ev.InnerEvent.Type != want || ev.APIAppID != "AFAKEAPP" || ev.TeamID != "TFAKE" {
			t.Errorf("request %d = %s %s for %s in %s; want an event_callback of %s for AFAKEAPP in TFAKE", i, ev.Type, ev.InnerEvent.Type, ev.APIAppID, ev.TeamID, want)
		}
		if got[i].header.Get("X-Slack-Retry-Num") != "" || got[i].header.Get("X-Slack-Retry-Reason") != "" {
			t.Errorf("request %d carries retry headers on its first attempt: %v", i, got[i].header)
		}
	}
	if payload, acked := fake.Ack(m.Envelopes[0]); !acked || payload != nil {
		t.Errorf("Ack = %s, %v; want acked with no payload for a 200 with no body", payload, acked)
	}
}

func TestHTTPDeliveryPostsAPressAsAFormWithItsPayload(t *testing.T) {
	rc := newReceiver(t, nil)
	fake := httpFake(t, rc.URL)
	root, ask := postAsk(t, fake)

	id, err := fake.Click("U1", ask, "approve", "yes")
	if err != nil {
		t.Fatal(err)
	}
	waitAcked(t, fake, id)
	r := rc.wait(t, 1)[0]
	verifySigned(t, r)
	if ct := r.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q; want a form", ct)
	}
	form, err := url.ParseQuery(string(r.body))
	if err != nil || len(form) != 1 {
		t.Fatalf("body = %q (%v); want a form with the field payload alone", r.body, err)
	}
	var cb slack.InteractionCallback
	if err := json.Unmarshal([]byte(form.Get("payload")), &cb); err != nil {
		t.Fatalf("payload is not an interaction: %v", err)
	}
	act := cb.ActionCallback.BlockActions
	if cb.Type != slack.InteractionTypeBlockActions || cb.User.ID != "U1" || cb.Container.MessageTs != ask || cb.Container.ThreadTs != root ||
		len(act) != 1 || act[0].ActionID != "approve" || act[0].Value != "yes" {
		t.Errorf("payload = %+v; want the block_actions press on approve in %s", cb, ask)
	}
}

func TestHTTPDeliveryRetriesAsSlackDoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fail   func(w http.ResponseWriter) // how each of the first two attempts fails
		reason string
	}{
		{"a 503", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, "http_error"},
		{"a 500", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, "http_error"},
		{"a connection that closes with no answer", func(w http.ResponseWriter) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, "connection_failed"},
		{"no answer within the timeout", func(http.ResponseWriter) { time.Sleep(300 * time.Millisecond) }, "http_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newReceiver(t, func(n int, _ received, w http.ResponseWriter) {
				if n < 2 {
					tc.fail(w)
				}
			})
			fake := httpFake(t, rc.URL, fakeslack.WithHTTPTimeout(100*time.Millisecond))

			m, err := fake.Say("C1", "U1", "hello")
			if err != nil || len(m.Envelopes) != 1 {
				t.Fatalf("Say = %+v, %v", m, err)
			}
			waitAcked(t, fake, m.Envelopes...)
			got := rc.wait(t, 3)
			for i, r := range got {
				verifySigned(t, r) // each attempt is signed afresh
				if !bytes.Equal(r.body, got[0].body) {
					t.Errorf("attempt %d sent another body: %s; want the first's", i, r.body)
				}
				num, reason := r.header.Get("X-Slack-Retry-Num"), r.header.Get("X-Slack-Retry-Reason")
				switch {
				case i == 0 && (num != "" || reason != ""):
					t.Errorf("the first attempt has X-Slack-Retry-Num %q and X-Slack-Retry-Reason %q; want neither", num, reason)
				case i > 0 && (num != strconv.Itoa(i) || reason != tc.reason):
					t.Errorf("attempt %d has X-Slack-Retry-Num %q and X-Slack-Retry-Reason %q; want %d and %s", i, num, reason, i, tc.reason)
				}
			}
			time.Sleep(50 * time.Millisecond)
			if n := rc.count(); n != 3 {
				t.Errorf("the receiver got %d requests; want 3, the last of them answered 200", n)
			}
		})
	}
}

func TestHTTPDeliveryGivesUpAfterTheRetrySchedule(t *testing.T) {
	rc := newReceiver(t, func(_ int, _ received, w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	fake := httpFake(t, rc.URL, fakeslack.WithHTTPRetries(5*time.Millisecond, 5*time.Millisecond))

	m, err := fake.Say("C1", "U1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	rc.wait(t, 3)
	time.Sleep(100 * time.Millisecond)
	if n := rc.count(); n != 3 {
		t.Errorf("the receiver got %d requests; want the first and two retries", n)
	}
	if _, acked := fake.Ack(m.Envelopes[0]); acked {
		t.Error("a delivery that was never answered 200 is acked")
	}
}

func TestHTTPDeliveryDoesNotRetryAClientError(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			rc := newReceiver(t, func(_ int, _ received, w http.ResponseWriter) { w.WriteHeader(code) })
			fake := httpFake(t, rc.URL)

			m, err := fake.Say("C1", "U1", "hello")
			if err != nil {
				t.Fatal(err)
			}
			rc.wait(t, 1)
			time.Sleep(100 * time.Millisecond)
			if n := rc.count(); n != 1 {
				t.Errorf("the receiver got %d requests; want one, not retried", n)
			}
			if _, acked := fake.Ack(m.Envelopes[0]); acked {
				t.Errorf("a delivery answered %d is acked", code)
			}
		})
	}
}

func TestHTTPDeliveryKeepsTheOrderOfAChannelAndHoldsUpNoOther(t *testing.T) {
	release := make(chan struct{})
	rc := newReceiver(t, func(_ int, r received, w http.ResponseWriter) {
		if strings.Contains(string(r.body), `"text":"first"`) {
			select {
			case <-release:
			default:
				w.WriteHeader(http.StatusServiceUnavailable) // retried until the test lets it through
			}
		}
	})
	fake := httpFake(t, rc.URL, fakeslack.WithHTTPRetries(20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond))

	first, err := fake.Say("C1", "U1", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := fake.Say("C1", "U1", "second")
	if err != nil {
		t.Fatal(err)
	}
	other, err := fake.Say("C2", "U1", "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	waitAcked(t, fake, other.Envelopes...)
	if _, acked := fake.Ack(second.Envelopes[0]); acked {
		t.Fatal("the second message of C1 was acked while the first was still being retried")
	}
	close(release)
	waitAcked(t, fake, first.Envelopes[0], second.Envelopes[0])
	var order []string
	for _, r := range rc.wait(t, rc.count()) {
		inner := callback(t, r).InnerEvent.Data.(*slackevents.MessageEvent)
		if inner.Channel == "C1" {
			order = append(order, inner.Text)
		}
	}
	if last := order[len(order)-1]; last != "second" || strings.Count(strings.Join(order, ","), "second") != 1 {
		t.Errorf("C1's requests came as %v; want every attempt of first before second, sent once", order)
	}
}

func TestHTTPDeliveryIsNotSentForARevokedApp(t *testing.T) {
	rc := newReceiver(t, nil)
	fake := httpFake(t, rc.URL)
	if err := fake.SetRevoked("AFAKEAPP", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.Say("C1", "U1", "hello"); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Errorf("Say for a revoked app in HTTP mode = %v; want ErrNoConnection, as for a revoked app's closed sockets", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := rc.count(); n != 0 {
		t.Errorf("the receiver got %d requests for a revoked app; want none", n)
	}
}

func TestHTTPDeliveryRefusesAnEnvelopeTypeSlackDoesNotSendOverHTTPAsAnEnvelope(t *testing.T) {
	rc := newReceiver(t, nil)
	fake := httpFake(t, rc.URL)
	if _, err := fake.SendEnvelope("slash_commands", map[string]any{"command": "/x"}); !errors.Is(err, fakeslack.ErrInvalid) {
		t.Errorf("SendEnvelope(slash_commands) in HTTP mode = %v; want ErrInvalid", err)
	}
	id, err := fake.SendEnvelope("events_api", map[string]any{"type": "event_callback", "event": map[string]any{"type": "message", "channel": "C1"}})
	if err != nil {
		t.Fatal(err)
	}
	waitAcked(t, fake, id)
}

func TestHTTPDeliveryStopsAtClose(t *testing.T) {
	leakCheck(t)
	rc := newReceiver(t, func(_ int, _ received, w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	fake := fakeslack.New(fakeslack.WithRequestURL(rc.URL, testSigningSecret), fakeslack.WithHTTPRetries(time.Hour))
	if _, err := fake.Say("C1", "U1", "hello"); err != nil {
		t.Fatal(err)
	}
	rc.wait(t, 1) // the delivery now waits an hour to retry
	done := make(chan struct{})
	go func() { fake.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end a delivery waiting to retry")
	}
	if _, err := fake.Say("C1", "U1", "after"); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Errorf("Say after Close = %v; want ErrNoConnection", err)
	}
}

func TestVerifyURLSendsAURLVerificationAndChecksTheChallenge(t *testing.T) {
	echo := func(_ int, r received, w http.ResponseWriter) {
		var v struct{ Type, Challenge string }
		if err := json.Unmarshal(r.body, &v); err != nil || v.Type != "url_verification" || v.Challenge == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": v.Challenge})
	}
	for _, tc := range []struct {
		name   string
		answer func(n int, r received, w http.ResponseWriter)
		ok     bool
	}{
		{"the challenge echoed", echo, true},
		{"another challenge", func(_ int, _ received, w http.ResponseWriter) { _, _ = io.WriteString(w, `{"challenge":"nope"}`) }, false},
		{"a 503", func(_ int, _ received, w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, false},
		{"a 200 with no body", func(int, received, http.ResponseWriter) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newReceiver(t, tc.answer)
			fake := httpFake(t, rc.URL)
			err := fake.VerifyURL(t.Context(), "AFAKEAPP")
			if (err == nil) != tc.ok {
				t.Fatalf("VerifyURL = %v; want ok %v", err, tc.ok)
			}
			r := rc.wait(t, 1)[0]
			verifySigned(t, r)
			if rc.count() != 1 {
				t.Errorf("VerifyURL sent %d requests; want one, never retried", rc.count())
			}
			var conns struct {
				Connections int `json:"connections"`
			}
			if s := control(t, fake, http.MethodGet, "/_control/connections?app=AFAKEAPP", nil, &conns); s != http.StatusOK {
				t.Fatalf("GET /_control/connections = %d", s)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.ok]; conns.Connections != want {
				t.Errorf("connections of an app in HTTP mode = %d; want %d, whether its URL answers a url_verification", conns.Connections, want)
			}
		})
	}
}

func TestVerifyURLRefusesAnAppNotInHTTPMode(t *testing.T) {
	fake, _ := newFake(t)
	if err := fake.VerifyURL(t.Context(), "AFAKEAPP"); !errors.Is(err, fakeslack.ErrInvalid) {
		t.Errorf("VerifyURL of a Socket Mode app = %v; want ErrInvalid", err)
	}
	if err := fake.VerifyURL(t.Context(), "ANOPE"); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("VerifyURL of no app = %v; want ErrNotFound", err)
	}
}

func TestHTTPDeliveryGoesToEachAppsOwnURLWithItsOwnSecret(t *testing.T) {
	rcDefault, rcTeam := newReceiver(t, nil), newReceiver(t, nil)
	team := fakeslack.App{AppID: "ATEAM", BotUser: "UTEAM", BotID: "BTEAM", BotToken: "xoxb-team-token", RequestURL: rcTeam.URL, SigningSecret: "fake-team-secret"}
	fake := httpFake(t, rcDefault.URL, fakeslack.WithApp(team))
	if err := fake.Join("C1", "ATEAM"); err != nil {
		t.Fatal(err)
	}
	m, err := fake.Say("C1", "U1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitAcked(t, fake, m.Envelopes...)
	verifySigned(t, rcDefault.wait(t, 1)[0])
	r := rcTeam.wait(t, 1)[0]
	sv, err := slack.NewSecretsVerifier(r.header, "fake-team-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sv.Write(r.body)
	if err := sv.Ensure(); err != nil {
		t.Errorf("the team app's request is not signed with its own secret: %v", err)
	}
	if ev := callback(t, r); ev.APIAppID != "ATEAM" {
		t.Errorf("the team app's URL got an event for %s", ev.APIAppID)
	}
}

func TestAnHTTPAppsRequestURLAndSigningSecretAreBothOrNeither(t *testing.T) {
	base := fakeslack.App{AppID: "A1", BotUser: "U1", BotID: "B1"}
	for _, tc := range []struct {
		name        string
		url, secret string
		ok          bool
	}{
		{"neither: Socket Mode", "", "", true},
		{"both", "http://broker.example:8081/chat/slack/ns/name", "fake-secret-value", true},
		{"a URL without a secret", "http://broker.example:8081/x", "", false},
		{"a secret without a URL", "", "fake-secret-value", false},
		{"a relative URL", "/chat/slack/ns/name", "fake-secret-value", false},
		{"another scheme", "ftp://broker.example/x", "fake-secret-value", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			a.RequestURL, a.SigningSecret = tc.url, tc.secret
			err := a.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v; want ok %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "fake-secret-value") {
				t.Errorf("Validate's error %q repeats the signing secret", err)
			}
			if got := fakeslack.CheckRequestURL(tc.url, tc.secret); (got == nil) != tc.ok {
				t.Errorf("CheckRequestURL = %v; want ok %v", got, tc.ok)
			}
		})
	}
}

func TestTheControlSurfaceNeverShowsASigningSecret(t *testing.T) {
	rc := newReceiver(t, nil)
	fake := httpFake(t, rc.URL)
	var apps []map[string]any
	if s := control(t, fake, http.MethodGet, "/_control/apps", nil, &apps); s != http.StatusOK || len(apps) != 1 {
		t.Fatalf("GET /_control/apps = %d, %v", s, apps)
	}
	raw, _ := json.Marshal(apps)
	if strings.Contains(string(raw), testSigningSecret) {
		t.Errorf("/_control/apps shows the signing secret: %s", raw)
	}
	if apps[0]["request_url"] != rc.URL {
		t.Errorf("/_control/apps shows request_url %v; want %s", apps[0]["request_url"], rc.URL)
	}
}

// waitCtx is a context for a wait in a test, ended with the test.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}
