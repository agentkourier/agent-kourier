package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.opentelemetry.io/otel/attribute"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// What Slack's requests are, in HTTP mode, is Slack's (upstream, not verified here): the headers below, a timestamp in Unix seconds, a
// signature "v0=" and the hex of HMAC-SHA256 with the app's signing secret over "v0:<timestamp>:<body>", an Events API payload sent as
// JSON, a press sent form-encoded with its JSON in the field payload, and a retry of a request that was not answered 200 within about
// 3 seconds.
const (
	// httpAckWait is how long the handler waits for the sink to take an event or a press, from when it was given the request, before it
	// answers 503 for Slack to send it again. Slack waits about 3 seconds for the answer, counted from when it sent the request, and the
	// chat listener has spent some of them reading it: the rest is for the network.
	httpAckWait = 2 * time.Second
	// signatureMaxAge is how far the timestamp of a request may be from the Client's clock, either way, for the request to be taken:
	// Slack's advice. A signed request that is replayed later than that is refused. slack-go's verifier, which checks the signature after
	// this, refuses past the same age on its own.
	signatureMaxAge = 5 * time.Minute

	headerTimestamp = "X-Slack-Request-Timestamp"
	headerRetryNum  = "X-Slack-Retry-Num"
)

// The outcomes of a request in HTTP mode, the label of agentkourier_slack_http_requests_total: a closed set.
const (
	httpAcknowledged = "acknowledged"  // 200: the sink took the event or the press, or it was nothing to hand on
	httpChallenge    = "challenge"     // 200: a url_verification, answered with its challenge
	httpBadSignature = "bad_signature" // 401: the signature or the timestamp is missing or unreadable, or the signature does not match
	httpStale        = "stale"         // 401: the timestamp is further than signatureMaxAge from the clock
	httpMalformed    = "malformed"     // 400: signed, and no event or press the adapter can read
	httpNotRunning   = "not_running"   // 503: no Run takes requests (none has started, one is between attempts, or the last has ended)
	httpShed         = "shed"          // 503: the lanes hold as many events as they may (agentkourier_slack_events_shed_total)
	httpWithheld     = "withheld"      // 503: the sink did not take it
	httpTimeout      = "timeout"       // 503: the sink had not answered within httpAckWait, or by the request's own deadline
)

// httpRun is a Run in HTTP mode, as the handler of HTTPHandler finds it (Client.http). The handler submits to its lanes under mu held for
// reading, and Run closes it under mu held for writing before it waits for its lanes, so that nothing is submitted while it waits.
type httpRun struct {
	*runState
	// ctx is Run's, which ends when Run does. The handler calls the sink with it, not with the request's, and counts on it, since it
	// carries the broker's instruments.
	ctx    context.Context
	mu     sync.RWMutex
	closed bool // guarded by mu
}

// submit queues fn on the lane of channel (lanes.submit, whose refusal is reason), unless the run has ended: open is false then, and
// nothing is queued. It never blocks on fn.
func (r *httpRun) submit(channel string, fn func()) (reason string, open bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return "", false
	}
	return r.lanes.submit(channel, fn), true
}

// serveHTTP is Run in HTTP mode. It opens no connection: Slack sends its requests to the handler of HTTPHandler, which the broker serves
// on its chat listener, and serveHTTP makes r the run the handler hands them to, says the transport is up, and holds it until ctx, Run's
// and r's, ends. It then closes r, so that a request that comes after (the broker may serve the route a moment longer) is answered 503,
// and says the transport is down.
func (c *Client) serveHTTP(ctx context.Context, r *httpRun) error {
	c.http.Store(r)
	if c.onConn != nil {
		c.onConn(true)
	}
	c.log.InfoContext(ctx, "slack: http mode taking requests")
	<-ctx.Done()
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	if c.onConn != nil {
		c.onConn(false)
	}
	return fmt.Errorf("slack http mode: %w", ctx.Err())
}

// HTTPHandler implements chat.HTTPReceiver: in HTTP mode, the handler of every request Slack sends to the connection, events and
// presses alike (serveRequest), which is the same for each Run of the Client; in Socket Mode none, and false.
func (c *Client) HTTPHandler() (http.Handler, bool) {
	if !c.overHTTP() {
		return nil, false
	}
	return http.HandlerFunc(c.serveRequest), true
}

var _ chat.HTTPReceiver = (*Client)(nil)

// serveRequest answers one request from Slack. It reads nothing of the body before the signature is checked (verify), and answers 401 if
// the request is not Slack's. A url_verification is answered with its challenge, whether or not a Run is live. An event or a press is
// handed to the lanes of the live Run, as an envelope is in Socket Mode, and the answer waits for the sink: 200 once it took it (or there
// was nothing to hand on), and 503 when no Run is live, the lanes are full, the sink did not take it, or it has not answered within
// httpAckWait. Slack sends again what it was not answered 200.
//
// Nothing on this path calls Slack or an agent, so the answer comes as soon as the sink returns: in milliseconds when the sink is quick,
// which the chat listener's cap on the requests in flight counts on. The sink is called with the Run's context, not the request's, as in
// Socket Mode, so an event whose answer timed out may still be taken: Slack's retry of it is then a duplicate, which the intake drops if
// the first had finished. An event still waiting on its lane when the answer goes is not given to the sink.
func (c *Client) serveRequest(w http.ResponseWriter, req *http.Request) {
	received := time.Now()
	run := c.http.Load()
	// The series are counted on the Run's context, which carries the broker's instruments: a request that comes before the first Run
	// is not counted.
	tctx := req.Context()
	if run != nil {
		tctx = run.ctx //nolint:contextcheck // the run's context carries the broker's instruments, and the request's does not
	}
	answer := func(outcome string, code int) {
		telemetry.Count(tctx, "agentkourier_slack_http_requests_total", outcome, c.conn)
		if code == http.StatusOK {
			w.WriteHeader(code)
			return
		}
		http.Error(w, http.StatusText(code), code)
	}
	body, err := io.ReadAll(req.Body) // the chat listener has read it whole and capped it: this reads memory
	if err != nil {
		answer(httpMalformed, http.StatusBadRequest)
		return
	}
	if outcome := c.verify(req.Header, body); outcome != "" {
		c.log.DebugContext(tctx, "slack: a request was refused: it is not signed by the connection's app", "outcome", outcome)
		answer(outcome, http.StatusUnauthorized)
		return
	}
	if req.Header.Get(headerRetryNum) != "" {
		telemetry.Count(tctx, "agentkourier_slack_http_retries_total", c.conn)
	}
	d, challenge, err := decodeRequest(req.Header.Get("Content-Type"), body)
	if err != nil {
		// What failed to decode is Slack's, signed, and may hold what people wrote: the log says what it was and not what it said.
		c.log.WarnContext(tctx, "slack: a signed request could not be read; answered 400",
			"contentType", req.Header.Get("Content-Type"), "bytes", len(body), "reason", err.Error())
		answer(httpMalformed, http.StatusBadRequest)
		return
	}
	if challenge != "" {
		telemetry.Count(tctx, "agentkourier_slack_http_requests_total", httpChallenge, c.conn)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackevents.ChallengeResponse{Challenge: challenge})
		return
	}
	if run == nil {
		answer(httpNotRunning, http.StatusServiceUnavailable)
		return
	}
	// What waits on the lane holds the decoded delivery and the request's context, and not the request: once the handler has answered,
	// nothing keeps its body.
	result, answered := make(chan error, 1), req.Context()
	channel := d.channel(run.me)
	reason, open := run.submit(channel, func() {
		if answered.Err() != nil || run.ctx.Err() != nil {
			return // the answer has gone, or Run has ended: Slack sends it again
		}
		ctx, op := telemetry.Start(run.ctx, "agentkourier.slack.envelope", attribute.String("agentkourier.platform", "slack"))
		err := c.deliver(ctx, run.me, run.sink, d) //nolint:contextcheck // the sink is called on the Run's context, as in Socket Mode (above)
		if err != nil {
			op.FinishOutcome(nil, httpWithheld)
		} else {
			op.FinishOutcome(nil, httpAcknowledged)
		}
		result <- err
	})
	switch {
	case !open:
		answer(httpNotRunning, http.StatusServiceUnavailable)
		return
	case reason != "":
		c.shed(run.ctx, run.shed, reason, channel, d.kind)
		answer(httpShed, http.StatusServiceUnavailable)
		return
	}
	timer := time.NewTimer(c.ackWait)
	defer timer.Stop()
	outcome, code := httpTimeout, http.StatusServiceUnavailable
	select {
	case err := <-result:
		outcome, code = httpAcknowledged, http.StatusOK
		if err != nil {
			outcome, code = httpWithheld, http.StatusServiceUnavailable
		}
	case <-timer.C:
	case <-req.Context().Done():
	case <-run.ctx.Done():
		outcome = httpNotRunning
	}
	ack := "withheld"
	if code == http.StatusOK {
		ack = "acknowledged"
	}
	telemetry.Observe(run.ctx, "agentkourier_slack_ack_duration_seconds", time.Since(received), ack, c.conn)
	answer(outcome, code)
}

// verify checks that a request is Slack's for the connection's app, before anything of its body is read: its timestamp is within
// signatureMaxAge of the clock, and its signature is the one the signing secret makes of the timestamp and the body, compared in
// constant time (slack-go's SecretsVerifier). It returns "" for a request that passes, and the outcome it is refused with otherwise.
func (c *Client) verify(h http.Header, body []byte) string {
	ts, err := strconv.ParseInt(h.Get(headerTimestamp), 10, 64)
	if err != nil {
		return httpBadSignature
	}
	if age := c.now().Sub(time.Unix(ts, 0)); age > signatureMaxAge || age < -signatureMaxAge {
		return httpStale
	}
	sv, err := slackgo.NewSecretsVerifier(h, c.signing.Reveal())
	if errors.Is(err, slackgo.ErrExpiredTimestamp) {
		return httpStale
	}
	if err != nil {
		return httpBadSignature
	}
	if _, err := sv.Write(body); err != nil || sv.Ensure() != nil {
		return httpBadSignature
	}
	return ""
}

// decodeRequest reads the body of a signed request: a form with the field payload is a press, and anything else an Events API payload in
// JSON. It returns the delivery, or the challenge of a url_verification, or why the body cannot be read, which quotes none of it. An
// Events API payload that is neither (app_rate_limited) is a delivery of nothing, which is acknowledged.
func decodeRequest(contentType string, body []byte) (d delivery, challenge string, err error) {
	if mt, _, _ := mime.ParseMediaType(contentType); mt == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil || form.Get("payload") == "" {
			return delivery{}, "", errors.New("a form with no payload")
		}
		var cb slackgo.InteractionCallback
		if err := json.Unmarshal([]byte(form.Get("payload")), &cb); err != nil {
			return delivery{}, "", errors.New("a payload that is not an interaction")
		}
		return delivery{kind: string(socketmode.EventTypeInteractive), press: &cb}, "", nil
	}
	outer, err := slackevents.ParseEvent(body, slackevents.OptionNoVerifyToken())
	if err != nil {
		return delivery{}, "", errors.New("no Events API payload slack-go can parse")
	}
	d = delivery{kind: string(socketmode.EventTypeEventsAPI)}
	switch outer.Type {
	case slackevents.URLVerification:
		v, ok := outer.Data.(*slackevents.EventsAPIURLVerificationEvent)
		if !ok || v.Challenge == "" {
			return delivery{}, "", errors.New("a url_verification with no challenge")
		}
		return delivery{}, v.Challenge, nil
	case slackevents.CallbackEvent:
		d.event = &outer
	}
	return d, "", nil
}
