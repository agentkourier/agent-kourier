package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// errNotRunning says that no Run takes the event: none has started, or the one that took it has ended.
var errNotRunning = errors.New("web: the connection is not running here")

// HTTPHandler implements chat.HTTPReceiver: every request of a web connection comes over HTTP, so there is always a handler, the same
// for each Run.
func (p *Platform) HTTPHandler() (http.Handler, bool) { return http.HandlerFunc(p.serve), true }

// serve answers one request. Nothing of the body is decoded before the profile has verified it. The answer is:
//
//   - 202 for an event the Sink took, and for a request that carries nothing to take. A repeat of an event is taken too: the Sink
//     drops it, by its ID;
//   - 400 for a body that is not the profile's schema, or an event the platform cannot take (no channel or ID, or a ':' in one or in
//     the thread);
//   - 401 for a request the profile does not verify, which says nothing of why;
//   - 405 for a method other than POST;
//   - 413 for a body over the profile's bound;
//   - 503 with a Retry-After of busyRetryAfter, which the sender sends again, when no Run takes events, the Sink did not take the
//     event, or it had not answered within the wait (ackWait).
//
// The Sink is called on a context of Run's, not the request's, since the broker's telemetry is in Run's, and the context ends when the
// answer goes: an event is not taken after its sender was told to send it again. What a request holds (its body, its signature) is
// never logged.
//
// Every answer but the 405, which the chat listener gives before a request reaches here, is counted as agentkourier_web_requests_total
// by its outcome, on the latest Run's context, which carries the broker's instruments: a request that comes before the first Run is
// not counted.
func (p *Platform) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	tctx := req.Context()
	if r := p.last.Load(); r != nil {
		tctx = r.ctx //nolint:contextcheck // the run's context carries the broker's instruments, and the request's does not
	}
	answer := func(outcome string, code int) {
		telemetry.Count(tctx, "agentkourier_web_requests_total", outcome, string(p.profile.Name()), p.conn)
		switch code {
		case http.StatusAccepted:
			w.WriteHeader(code)
			return
		case http.StatusServiceUnavailable:
			w.Header().Set("Retry-After", strconv.Itoa(int(busyRetryAfter/time.Second)))
		}
		http.Error(w, http.StatusText(code), code)
	}
	body, err := io.ReadAll(req.Body) // the chat listener has read it whole and capped it: this reads memory
	if err != nil {
		answer(outcomeMalformed, http.StatusBadRequest)
		return
	}
	if err := p.profile.Verify(req.Header, body, p.now()); err != nil {
		p.log.DebugContext(req.Context(), "web: a request was refused: the profile did not verify it", "connection", p.conn, "reason", err.Error())
		answer(outcomeUnverified, http.StatusUnauthorized)
		return
	}
	ev, ok, err := p.profile.Decode(req.Header, body)
	switch {
	case errors.Is(err, ErrTooLarge):
		p.log.WarnContext(req.Context(), "web: a signed request was refused: its body is over the profile's bound", "connection", p.conn, "bytes", len(body))
		answer(outcomeTooLarge, http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		// The body is the sender's, signed, and may hold what people wrote: the log says how much there was and why it was refused.
		p.log.WarnContext(req.Context(), "web: a signed request was refused: it is not the profile's schema", "connection", p.conn, "bytes", len(body), "reason", err.Error())
		answer(outcomeMalformed, http.StatusBadRequest)
		return
	case !ok:
		answer(outcomeIgnored, http.StatusAccepted)
		return
	case !takeable(ev):
		p.log.ErrorContext(req.Context(), "web: the profile decoded an event with no channel or ID, or a ':' in one or in the thread; refused", "connection", p.conn)
		answer(outcomeMalformed, http.StatusBadRequest)
		return
	}
	m := chat.Inbound{At: p.RefAt(ev.Channel, ev.Thread, ev.ID), User: ev.User, Text: ev.Text, Mentioned: ev.Mentioned}
	switch err := p.take(req.Context(), m); {
	case errors.Is(err, errNotRunning):
		answer(outcomeNotRunning, http.StatusServiceUnavailable)
	case err != nil:
		p.log.WarnContext(req.Context(), "web: an event was not taken; the sender is told to send it again", "connection", p.conn, "channel", ev.Channel, "event", ev.ID, "err", err)
		outcome := outcomeWithheld
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			outcome = outcomeTimeout
		}
		answer(outcome, http.StatusServiceUnavailable)
	default:
		answer(outcomeAccepted, http.StatusAccepted)
	}
}

// The outcomes of a request to a web connection, the label outcome of agentkourier_web_requests_total: a closed set.
const (
	outcomeAccepted   = "accepted"    // 202: the Sink took the event, or dropped it as a repeat
	outcomeIgnored    = "ignored"     // 202: the request carries nothing to take, such as a sender's ping
	outcomeUnverified = "unverified"  // 401: the profile did not verify it
	outcomeMalformed  = "malformed"   // 400: not the profile's schema, or an event the platform cannot take, or a body that could not be read
	outcomeTooLarge   = "too_large"   // 413: the body is over the profile's bound
	outcomeNotRunning = "not_running" // 503: no Run takes events (it ended, or none has started since the route was served)
	outcomeWithheld   = "withheld"    // 503: the Sink did not take the event
	outcomeTimeout    = "timeout"     // 503: the Sink had not answered within ackWait, or the request ended first
)

// takeable says whether the platform can hand ev on. chat.MessageID joins the channel and the ID with ':', and the session's notice
// keys join the channel and the thread with it, so that two events whose parts held a ':' could be one key, and the second dropped as
// a repeat of the first.
func takeable(ev Event) bool {
	return ev.Channel != "" && ev.ID != "" && !strings.Contains(ev.Channel, ":") && !strings.Contains(ev.ID, ":") && !strings.Contains(ev.Thread, ":")
}

// take hands m to the live Run's Sink, after the events before it, and returns the Sink's answer: errNotRunning when no Run takes it,
// and the context's error when the wait (ackWait), the request or the Run ends first.
func (p *Platform) take(reqCtx context.Context, m chat.Inbound) error {
	r := p.run.Load()
	if p.afterLoad != nil {
		p.afterLoad()
	}
	if r == nil {
		return errNotRunning
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return errNotRunning
	}
	ctx, cancel := context.WithTimeout(r.ctx, p.ackWait) // the Run's values (the broker's telemetry), ended by the request too
	defer cancel()
	stop := context.AfterFunc(reqCtx, cancel)
	defer stop()
	select {
	case p.order <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.order }()
	return r.sink.Message(ctx, m) //nolint:contextcheck // the Sink is called on the Run's context, as Slack's HTTP mode does
}
