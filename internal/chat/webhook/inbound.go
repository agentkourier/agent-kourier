package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The outcomes of a request to a webhook connection, the label outcome of agentkourier_webhook_requests_total: a closed set.
const (
	outcomeAccepted   = "accepted"    // 200: the Sink took the event, or dropped it as a repeat
	outcomeIgnored    = "ignored"     // 200: no actor resolved, and the connection keeps no such event
	outcomeSelf       = "self"        // 200: the agent's own event, dropped
	outcomeUnmapped   = "unmapped"    // 200: the event has no channel, or no thread the connection can use
	outcomeUnverified = "unverified"  // 401: not the sender's
	outcomeMalformed  = "malformed"   // 400: the body could not be read, or is not one JSON object within the bounds
	outcomeNotRunning = "not_running" // 503: no Run takes events (none has started, or the one that took it has ended)
	outcomeWithheld   = "withheld"    // 503: the Sink did not take the event
	outcomeTimeout    = "timeout"     // 503: the Sink had not taken it within ackWait, or by the request's deadline
	outcomeCancelled  = "cancelled"   // 503: the sender went away before the Sink had taken it
)

// The reasons of an outcome, the label reason of agentkourier_webhook_requests_total: a closed set for each outcome, and reasonNone
// for an outcome that has no other.
const (
	reasonNone         = "none"
	reasonUnsigned     = "unsigned"      // unverified: the verify header is missing or empty
	reasonBadSignature = "bad_signature" // unverified: the header does not hold the prefix and the body's HMAC, or the token
	reasonStale        = "stale"         // unverified: the body timestamp is missing, not an integer, or outside the window
	reasonNoChannel    = "no_channel"    // unmapped: no channel pointer resolves, or its value is not a channel's shape
	reasonBadID        = "bad_id"        // unmapped: the thread's field resolves only to values that fail the id check
	reasonNoThread     = "no_thread"     // unmapped: the thread's field does not resolve, or is empty
	reasonNoActor      = "no_actor"      // ignored: no actor pointer resolves
	reasonUnreadable   = "unreadable"    // malformed: the body could not be read, before it was verified
	reasonSchema       = "schema"        // malformed: not one JSON object of valid UTF-8
	reasonDepth        = "depth"         // malformed: nested deeper than maxDepth
	reasonDuplicateKey = "duplicate_key" // malformed: a key on the way to a value the connection reads is in its object twice
)

// requestSeries is every outcome with each of its reasons: the series Run makes exist at zero.
var requestSeries = []result{
	{outcomeAccepted, reasonNone},
	{outcomeIgnored, reasonNoActor},
	{outcomeSelf, reasonNone},
	{outcomeUnmapped, reasonNoChannel}, {outcomeUnmapped, reasonBadID}, {outcomeUnmapped, reasonNoThread},
	{outcomeUnverified, reasonUnsigned}, {outcomeUnverified, reasonBadSignature}, {outcomeUnverified, reasonStale},
	{outcomeMalformed, reasonUnreadable}, {outcomeMalformed, reasonSchema}, {outcomeMalformed, reasonDepth}, {outcomeMalformed, reasonDuplicateKey},
	{outcomeNotRunning, reasonNone},
	{outcomeWithheld, reasonNone},
	{outcomeTimeout, reasonNone},
	{outcomeCancelled, reasonNone},
}

// HTTPHandler implements chat.HTTPReceiver: every request of a webhook connection comes over HTTP, so there is always a handler, the
// same for each Run.
func (p *Platform) HTTPHandler() (http.Handler, bool) { return http.HandlerFunc(p.serve), true }

// serve answers one request, through the steps of the package comment. Nothing of the body is parsed before it is verified. The Sink
// is called on a context of Run's, not the request's, since the broker's telemetry is in Run's, and the context ends when the answer
// goes: an event is not taken after its sender was told to send it again. What a request holds (its body, its signature) is never
// logged. A request that is not a POST, which the chat listener refuses before it reaches here, is answered 405 and not counted.
func (p *Platform) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	ctx, start := req.Context(), time.Now()
	body, err := io.ReadAll(req.Body) // the chat listener has read it whole and capped it: this reads memory
	if err != nil {
		p.log.DebugContext(ctx, "webhook: a request was refused: its body could not be read", "connection", p.conn)
		p.answer(w, result{outcomeMalformed, reasonUnreadable})
		return
	}
	if reason := p.v.verify(req.Header, body, p.now()); reason != "" {
		p.log.DebugContext(ctx, "webhook: a request was refused: it is not verified", "connection", p.conn, "reason", reason, "bytes", len(body))
		p.unverifiedSummary(ctx, reason)
		p.answer(w, result{outcomeUnverified, reason})
		return
	}
	sum := sha256.Sum256(body)
	id, delivery := hex.EncodeToString(sum[:]), p.deliveryID(req.Header)
	m, res := p.decode(req.Header, body, id, delivery)
	var takeErr error
	if res == (result{}) {
		res, takeErr = p.take(ctx, m)
	}
	attrs := []any{"connection", p.conn, "outcome", res.outcome, "reason", res.reason, "delivery", delivery, "event", id,
		"bytes", len(body), "duration_ms", time.Since(start).Milliseconds()}
	if takeErr != nil {
		attrs = append(attrs, "err", takeErr)
	}
	p.log.InfoContext(ctx, "webhook: delivery answered", attrs...)
	p.answer(w, res)
}

// answer counts res and writes its answer.
func (p *Platform) answer(w http.ResponseWriter, res result) {
	p.rec.Count(res.outcome, res.reason)
	code := http.StatusOK
	switch res.outcome {
	case outcomeUnverified:
		code = http.StatusUnauthorized
	case outcomeMalformed:
		code = http.StatusBadRequest
	case outcomeNotRunning, outcomeWithheld, outcomeTimeout, outcomeCancelled:
		code = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", strconv.Itoa(int(busyRetryAfter/time.Second)))
	}
	if code == http.StatusOK {
		w.WriteHeader(code)
		return
	}
	http.Error(w, http.StatusText(code), code)
}

// take hands m to the live Run's Sink, after the events before it, and returns the result: accepted once the Sink took it;
// not_running when no Run takes it, or the Run ended first; cancelled when the request was cancelled first (the sender went away);
// timeout when ackWait or the request's deadline passed first; and withheld, with the Sink's error, when the Sink refused it.
func (p *Platform) take(reqCtx context.Context, m chat.Inbound) (result, error) {
	r := p.run.Load()
	if p.afterLoad != nil {
		p.afterLoad()
	}
	if r == nil {
		return result{outcomeNotRunning, reasonNone}, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return result{outcomeNotRunning, reasonNone}, nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, p.ackWait) // the Run's values (the broker's telemetry), ended by the request too
	defer cancel()
	stop := context.AfterFunc(reqCtx, cancel)
	defer stop()
	err := p.hand(ctx, r.sink, m) //nolint:contextcheck // the Sink is called on the Run's context, as Slack's HTTP mode does
	switch {
	case err == nil:
		return result{outcomeAccepted, reasonNone}, nil
	case ctx.Err() == nil:
		return result{outcomeWithheld, reasonNone}, err
	case errors.Is(reqCtx.Err(), context.Canceled):
		return result{outcomeCancelled, reasonNone}, nil
	case r.ctx.Err() != nil:
		return result{outcomeNotRunning, reasonNone}, nil
	}
	return result{outcomeTimeout, reasonNone}, nil
}

// hand hands m to sink once the events before it in the connection's order have been, and returns the Sink's answer, or ctx's error
// if it ends first.
func (p *Platform) hand(ctx context.Context, sink chat.Sink, m chat.Inbound) error {
	select {
	case p.order <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.order }()
	return sink.Message(ctx, m)
}

// unverifiedLog sums a connection's unverified requests, by reason, for the WARN line.
type unverifiedLog struct {
	mu     sync.Mutex
	last   time.Time      // when the last WARN line was logged; zero before the first
	counts map[string]int // the requests by reason since then
}

// unverifiedSummary counts an unverified request with reason, and logs the WARN line that sums the connection's unverified requests
// since the last one, the first at once and the next no sooner than unverifiedLogEvery after it, by the Config's clock. A request that
// comes within that time is in the next line, which the first unverified request after it logs.
func (p *Platform) unverifiedSummary(ctx context.Context, reason string) {
	u := &p.unverified
	now := p.now()
	u.mu.Lock()
	if u.counts == nil {
		u.counts = map[string]int{}
	}
	u.counts[reason]++
	if !u.last.IsZero() && now.Sub(u.last) < unverifiedLogEvery {
		u.mu.Unlock()
		return
	}
	counts, since := u.counts, u.last
	u.counts, u.last = map[string]int{}, now
	u.mu.Unlock()
	attrs := []any{"connection", p.conn, reasonUnsigned, counts[reasonUnsigned], reasonBadSignature, counts[reasonBadSignature],
		reasonStale, counts[reasonStale]}
	if !since.IsZero() {
		attrs = append(attrs, "since", since)
	}
	p.log.WarnContext(ctx, "webhook: requests were refused: they are not verified", attrs...)
}
