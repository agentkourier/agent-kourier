package fakeslack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// UNCONFIRMED(HTTP-8): what Slack sends in HTTP mode, and how it retries, is read from Slack's docs and not seen from Slack: each
// request carries X-Slack-Request-Timestamp, the Unix seconds it was sent at, and X-Slack-Signature, "v0=" and the hex of HMAC-SHA256
// with the app's signing secret over "v0:<timestamp>:<body>"; an Events API callback is JSON, a press is a form whose field payload
// holds its JSON; a request that is not answered 200 within about 3 seconds is sent again a few times, with X-Slack-Retry-Num (1, 2,
// ...) and X-Slack-Retry-Reason, at Slack's own pace (immediately, a minute and five minutes later, as its docs say).
const (
	// DefaultHTTPTimeout is how long the fake waits for an app's answer before it counts the request as failed, unless
	// WithHTTPTimeout says otherwise: Slack's 3 seconds.
	DefaultHTTPTimeout = 3 * time.Second
	// maxAnswerBody is how much of an answer's body the fake reads: a url_verification's challenge. An ack's body is not read.
	maxAnswerBody = 64 << 10

	retryHTTPError        = "http_error"        // the app answered with a server error (5xx)
	retryHTTPTimeout      = "http_timeout"      // the app had not answered within the timeout
	retryConnectionFailed = "connection_failed" // the request could not be sent, or the connection closed with no answer
)

// WithRequestURL puts the default app in HTTP mode (App.RequestURL): its events and presses are sent to requestURL, signed with
// signingSecret, and not over Socket Mode. WithApp does the same for another app with the App's fields. Values that CheckRequestURL
// refuses make it panic when the Server is built, as WithApp does. Set at construction, read-only after.
func WithRequestURL(requestURL, signingSecret string) Option {
	return func(s *Server) {
		if err := CheckRequestURL(requestURL, signingSecret); err != nil {
			panic(err)
		}
		s.apps[0].RequestURL, s.apps[0].SigningSecret = requestURL, signingSecret
	}
}

// DefaultHTTPRetries is the fake's retry schedule unless WithHTTPRetries says otherwise: three retries, as Slack makes, at a pace a
// test can wait for (1, 5 and 30 seconds), which spans a takeover of the broker's leadership (about 15 seconds and the new leader's
// start). Each call returns a new slice.
func DefaultHTTPRetries() []time.Duration {
	return []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
}

// WithHTTPRetries sets how long the fake waits before each retry of a request an app in HTTP mode did not answer 200: one wait for
// each retry, so that the number of waits is the number of retries. The default is three, of 1, 5 and 30 seconds; none is no retry.
//
// DELIBERATE DEPARTURE from Slack, which waits about a minute and then five before its last two retries: a test cannot wait that long.
func WithHTTPRetries(waits ...time.Duration) Option {
	return func(s *Server) { s.web.retries = append([]time.Duration{}, waits...) }
}

// WithHTTPTimeout sets how long the fake waits for an app in HTTP mode to answer a request before it counts it as failed and retries
// it (with the reason http_timeout). The default is Slack's 3 seconds.
func WithHTTPTimeout(d time.Duration) Option { return func(s *Server) { s.web.timeout = d } }

// webDeliveries is the fake's delivery to the apps in HTTP mode. The first attempt of each request waits its turn in a queue of its
// app and channel, so that the requests of a channel reach the app in the order they were sent, as the envelopes of a Socket Mode
// connection do, and one that is unanswered holds up its channel and no other. A request that is to be retried leaves the queue: its
// retries go out when their waits are over, and the requests after it are sent meanwhile, as Slack sends a retry. A retry can so reach
// the app after a later request of its channel.
//
// DELIBERATE DEPARTURE from Slack, which sends its requests at once, in no promised order (upstream, unverified): the fake sends the
// first attempts of a channel in order so that a test sees the same order in either mode while nothing fails.
type webDeliveries struct {
	retries []time.Duration // set by WithHTTPRetries, read-only after construction
	timeout time.Duration   // set by WithHTTPTimeout, read-only after construction
	// onAck is told of each request an app answered 200: the Server records the ack, with no payload (sockets.settle).
	onAck func(id string)
	// revoked says whether an app is revoked (Server.isRevoked): a request of a revoked app is not sent, or sent again.
	revoked func(appID string) bool

	client *http.Client
	ctx    context.Context // ends at Close, which ends every delivery
	cancel context.CancelFunc
	wg     sync.WaitGroup // the goroutine of each queue, and of each request that is being retried

	mu      sync.Mutex
	queues  map[string][]*webRequest // by app and channel, the request being sent first; guarded by mu
	stopped bool                     // set by Close; guarded by mu
}

// webRequest is one event or press, sent to an app's request URL until it is answered 200 or the retries run out.
type webRequest struct {
	id          string // the envelope ID the fake gave it, which its ack is recorded under
	app         string // the AppID of the app it is sent to
	url, secret string
	contentType string
	body        []byte
}

// start makes the HTTP client and the context of the deliveries, once the options have run.
func (d *webDeliveries) start() {
	if d.retries == nil {
		d.retries = DefaultHTTPRetries()
	}
	if d.timeout <= 0 {
		d.timeout = DefaultHTTPTimeout
	}
	// A transport of its own, whose idle connections Close closes, so that a test's leak check does not find them.
	d.client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.queues = map[string][]*webRequest{}
}

// stop ends every delivery, those waiting to be retried too, refuses any more, and waits for the queues' goroutines.
func (d *webDeliveries) stop() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
	d.client.CloseIdleConnections()
}

// enqueue puts r at the end of the queue of key, and starts that queue's goroutine if it was empty. It is false once the Server is
// closing, and r is not sent.
func (d *webDeliveries) enqueue(key string, r *webRequest) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	d.queues[key] = append(d.queues[key], r)
	if len(d.queues[key]) == 1 {
		d.wg.Go(func() { d.drain(key) })
	}
	return true
}

// drain sends the first attempt of each request of the queue of key, one after the other, until it is empty, and hands a request
// that is to be retried to a goroutine of its own (retry). The request being sent stays at the head of the queue, so that enqueue
// starts no second goroutine for it.
func (d *webDeliveries) drain(key string) {
	d.mu.Lock()
	for len(d.queues[key]) > 0 {
		r := d.queues[key][0]
		d.mu.Unlock()
		if reason := d.attempt(r, 0, ""); reason != "" && len(d.retries) > 0 {
			d.wg.Go(func() { d.retry(r, reason) }) // this goroutine is counted in wg, so the count is not 0 here
		}
		d.mu.Lock()
		d.queues[key] = d.queues[key][1:]
	}
	delete(d.queues, key)
	d.mu.Unlock()
}

// retry sends r again after each wait of the retry schedule while it is not answered 200 and the failure is one Slack retries
// (retryReason). reason is why the first attempt failed. Close ends it, at once, and so does a revocation of its app, before the next
// attempt.
func (d *webDeliveries) retry(r *webRequest, reason string) {
	for n, wait := range d.retries {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-d.ctx.Done():
			t.Stop()
			return
		}
		if reason = d.attempt(r, n+1, reason); reason == "" {
			return
		}
	}
}

// attempt sends r once, as attempt number n (0 for the first; the retry headers carry n and reason after it), records the ack with
// onAck if it is answered 200, and returns why it failed if Slack would send it again (retryReason), or "" when there is nothing to
// retry: it was acked, Slack would not retry the failure, the Server is closing or the app is revoked.
func (d *webDeliveries) attempt(r *webRequest, n int, reason string) string {
	if d.ctx.Err() != nil || d.revoked(r.app) {
		return ""
	}
	code, _, err := d.post(d.ctx, r.url, r.secret, r.contentType, r.body, n, reason)
	if err == nil && code == http.StatusOK {
		d.onAck(r.id)
		return ""
	}
	if d.ctx.Err() != nil {
		return ""
	}
	return retryReason(code, err)
}

// retryReason is the X-Slack-Retry-Reason of the retry of a request that failed so, or "" for a failure Slack would not cure by sending
// it again, which the fake does not retry: an answer of 4xx, or any other that is not 200 and not a server error.
//
// DELIBERATE DEPARTURE from Slack, which is believed to retry any answer but 200 (upstream, unverified): a client error is the app
// refusing the request (a bad signature, say), and retrying it only slows a test of that refusal.
func retryReason(code int, err error) string {
	var ue *url.Error
	switch {
	case errors.As(err, &ue) && ue.Timeout():
		return retryHTTPTimeout
	case err != nil:
		return retryConnectionFailed
	case code >= 500:
		return retryHTTPError
	}
	return ""
}

// post sends body to an app's url once, signed with secret as Slack signs (sign), with the retry headers on a retry, and returns the
// status and the body of the answer, or why there was none. The wait for the answer is bounded by the timeout.
func (d *webDeliveries) post(ctx context.Context, rawURL, secret, contentType string, body []byte, attempt int, reason string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("fakeslack: a request to an app's URL: %w", err)
	}
	ts := time.Now().Unix()
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Slack-Signature", sign(secret, ts, body))
	if attempt > 0 {
		req.Header.Set("X-Slack-Retry-Num", strconv.Itoa(attempt))
		req.Header.Set("X-Slack-Retry-Reason", reason)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, answer, nil
}

// sign is Slack's signature of a request sent at ts whose body is body: "v0=" and the hex of HMAC-SHA256, keyed with the app's signing
// secret, over "v0:<ts>:<body>".
func sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%d:", ts)
	_, _ = mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// sendHTTP sends an envelope's payload to an app in HTTP mode, as Slack sends the same thing over HTTP: an events_api payload as its
// JSON, an interactive one as a form whose field payload holds it. It records the envelope and returns its ID at once; the request
// goes out on the queue of the app and lane (webDeliveries), and the ack is recorded when it is answered 200. Any other envelope type
// is ErrInvalid: the fake sends no slash command over HTTP. A revoked app is sent nothing, as a revoked app in Socket Mode has no
// connection, and that, like a Server that is closing, is ErrNoConnection; a request queued or waiting to be retried when its app is
// revoked is not sent again (attempt).
func (s *Server) sendHTTP(app App, lane, typ string, payload any) (string, error) {
	if s.isRevoked(app.AppID) {
		return "", fmt.Errorf("fakeslack: app %s is revoked: %w", app.AppID, ErrNoConnection)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("fakeslack: encoding a payload: %w", err)
	}
	r := &webRequest{app: app.AppID, url: app.RequestURL, secret: app.SigningSecret}
	switch typ {
	case "events_api":
		r.contentType, r.body = "application/json", raw
	case "interactive":
		r.contentType, r.body = "application/x-www-form-urlencoded", []byte(url.Values{"payload": {string(raw)}}.Encode())
	default:
		return "", invalid("fakeslack: an app in HTTP mode is sent events_api and interactive payloads, not %q", typ)
	}
	r.id = s.sock.issue()
	if !s.web.enqueue(app.AppID+"\x00"+lane, r) {
		return "", fmt.Errorf("fakeslack: closing: %w", ErrNoConnection)
	}
	return r.id, nil
}

// VerifyURL sends the app appID's request URL a url_verification, signed, as Slack does when the URL is saved in the app's settings,
// once and not retried, and returns nil if the answer is 200 with the challenge it was sent. An app in Socket Mode is ErrInvalid, and an
// app the Server does not host ErrNotFound.
//
// The broker answers it on the route of a connection that runs on the replica that leads, whether or not its Run takes events yet (or
// still: a connection whose auth.test fails restarts, and its route comes and goes). GET /_control/connections?app= counts an app in HTTP
// mode as connected when it answers.
func (s *Server) VerifyURL(ctx context.Context, appID string) error {
	app, ok := s.appByID(appID)
	switch {
	case !ok:
		return fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	case !app.overHTTP():
		return invalid("fakeslack: app %s is in Socket Mode: it has no request URL", appID)
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce) // crypto/rand.Read does not fail
	challenge := hex.EncodeToString(nonce)
	body, err := json.Marshal(map[string]string{"type": "url_verification", "challenge": challenge})
	if err != nil {
		return fmt.Errorf("fakeslack: encoding a url_verification: %w", err)
	}
	code, answer, err := s.web.post(ctx, app.RequestURL, app.SigningSecret, "application/json", body, 0, "")
	if err != nil {
		return fmt.Errorf("fakeslack: url_verification of app %s: %w", appID, err)
	}
	var got struct {
		Challenge string `json:"challenge"`
	}
	if code != http.StatusOK || json.Unmarshal(answer, &got) != nil || got.Challenge != challenge {
		return fmt.Errorf("fakeslack: url_verification of app %s was answered %d without its challenge", appID, code)
	}
	return nil
}
