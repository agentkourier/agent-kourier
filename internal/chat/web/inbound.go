package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
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
//   - 400 for a body that is not the profile's schema, or an event the platform cannot take (no channel or ID, or a ':' in one);
//   - 401 for a request the profile does not verify, which says nothing of why;
//   - 405 for a method other than POST;
//   - 413 for a body over the profile's bound;
//   - 503 with Retry-After, which the sender sends again, when no Run takes events, the Sink did not take the event, or it had not
//     answered within the wait (ackWait).
//
// The Sink is called on a context of Run's, not the request's, since the broker's telemetry is in Run's, and the context ends when the
// answer goes: an event is not taken after its sender was told to send it again. What a request holds (its body, its signature) is
// never logged.
func (p *Platform) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(req.Body) // the chat listener has read it whole and capped it: this reads memory
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if err := p.profile.Verify(req.Header, body, p.now()); err != nil {
		p.log.DebugContext(req.Context(), "web: a request was refused: the profile did not verify it", "connection", p.conn, "reason", err.Error())
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	ev, ok, err := p.profile.Decode(req.Header, body)
	switch {
	case errors.Is(err, ErrTooLarge):
		p.log.WarnContext(req.Context(), "web: a signed request was refused: its body is over the profile's bound", "connection", p.conn, "bytes", len(body))
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		// The body is the sender's, signed, and may hold what people wrote: the log says how much there was and why it was refused.
		p.log.WarnContext(req.Context(), "web: a signed request was refused: it is not the profile's schema", "connection", p.conn, "bytes", len(body), "reason", err.Error())
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	case !ok:
		w.WriteHeader(http.StatusAccepted)
		return
	case !takeable(ev):
		p.log.ErrorContext(req.Context(), "web: the profile decoded an event with no channel or ID, or a ':' in one; refused", "connection", p.conn)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	m := chat.Inbound{At: p.RefAt(ev.Channel, ev.Thread, ev.ID), User: ev.User, Text: ev.Text, Mentioned: ev.Mentioned}
	if err := p.take(req.Context(), m); err != nil {
		if !errors.Is(err, errNotRunning) {
			p.log.WarnContext(req.Context(), "web: an event was not taken; the sender is told to send it again", "connection", p.conn, "channel", ev.Channel, "event", ev.ID, "err", err)
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// takeable says whether the platform can hand ev on: chat.MessageID joins the channel and the ID with ':', so that two events whose
// IDs were joined from parts with a ':' in them could be one ID, and the second dropped as a repeat of the first.
func takeable(ev Event) bool {
	return ev.Channel != "" && ev.ID != "" && !strings.Contains(ev.Channel, ":") && !strings.Contains(ev.ID, ":")
}

// take hands m to the live Run's Sink, after the events before it, and returns the Sink's answer: errNotRunning when no Run takes it,
// and the context's error when the wait (ackWait), the request or the Run ends first.
func (p *Platform) take(reqCtx context.Context, m chat.Inbound) error {
	r := p.run.Load()
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
