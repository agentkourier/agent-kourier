package fakeslack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoConnection is returned when an envelope is sent while no Socket Mode
// client is connected; for an app in HTTP mode, when the app is revoked or the
// Server is closing, since it has no connection to lack.
var ErrNoConnection = errors.New("fakeslack: no Socket Mode client is connected")

// envelope is the fake's record of one envelope it sent.
type envelope struct {
	acked chan struct{} // closed on the first ack
	// sent is closed once the fake will send the envelope no more: at once over Socket Mode, where it never sends one again,
	// and for a request to an app in HTTP mode once it is answered 200, its retries have run out or end (webDeliveries).
	sent    chan struct{}
	payload json.RawMessage
}

// EnvelopeOption changes an envelope SendEnvelope sends.
type EnvelopeOption func(envelope map[string]any)

// AcceptsResponsePayload marks an envelope as one whose ack may carry a
// response payload, as Slack does for view submissions and slash commands.
func AcceptsResponsePayload() EnvelopeOption {
	return func(e map[string]any) { e["accepts_response_payload"] = true }
}

// SendEnvelope sends a Socket Mode envelope of the given type ("events_api",
// "interactive", "slash_commands", ...) carrying payload to the default app, and
// returns its envelope_id. It fails with ErrNoConnection if the default app has
// no client connected, or if the write to the one chosen fails. SendEnvelopeTo
// names the app.
//
// An app with several clients connected gets the envelope on its newest one, or
// on each in turn with WithSocketSpread.
//
// UNCONFIRMED(WP-04): that envelopes go to the newest connection only, where
// Slack spreads them over up to 10 (see WithSocketSpread); and that the fake never redelivers an
// envelope that is not acked, where Slack retries with retry_attempt set.
//
// An app in HTTP mode (App.RequestURL) is sent the payload as a request to its URL instead (sendHTTP): an
// "events_api" payload as JSON, an "interactive" one as a form, and any other type is ErrInvalid. The ID is
// the fake's own name for that request and its retries, which Ack and WaitAck take as they take an
// envelope's, and the options do not apply.
func (s *Server) SendEnvelope(typ string, payload any, opts ...EnvelopeOption) (string, error) {
	return s.sendEnvelope(s.defaultApp().AppID, "", typ, payload, opts...)
}

// SendEnvelopeTo is SendEnvelope for the app appID. It fails with ErrNotFound for
// an app the Server does not host.
func (s *Server) SendEnvelopeTo(appID, typ string, payload any, opts ...EnvelopeOption) (string, error) {
	if _, ok := s.appByID(appID); !ok {
		return "", fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	}
	return s.sendEnvelope(appID, "", typ, payload, opts...)
}

// sendEnvelope sends payload to the app appID: over its Socket Mode connection, or to its request URL in HTTP
// mode, where lane (the channel the payload is about) names the queue whose order it keeps (webDeliveries).
func (s *Server) sendEnvelope(appID, lane, typ string, payload any, opts ...EnvelopeOption) (string, error) {
	if app, _ := s.appByID(appID); app.overHTTP() {
		return s.sendHTTP(app, lane, typ, payload)
	}
	sk := &s.sock
	sk.mu.Lock()
	c := sk.pickLocked(appID, s.spread)
	if c == nil {
		sk.mu.Unlock()
		return "", ErrNoConnection
	}
	id := sk.issueLocked()
	sk.mu.Unlock()

	env := map[string]any{"envelope_id": id, "type": typ, "accepts_response_payload": false, "payload": payload}
	for _, o := range opts {
		o(env)
	}
	if err := c.write(env); err != nil {
		return "", fmt.Errorf("fakeslack: sending envelope %s: %w: %w", id, ErrNoConnection, err)
	}
	return id, nil
}

// pickLocked chooses the connection of the app that gets the next envelope: its
// newest, or with spread the next in turn, oldest first. It is nil when the app
// has none. The caller holds mu.
func (sk *sockets) pickLocked(appID string, spread bool) *sockConn {
	var mine []*sockConn
	for _, c := range sk.conns {
		if c.app == appID {
			mine = append(mine, c)
		}
	}
	switch {
	case len(mine) == 0:
		return nil
	case !spread:
		return mine[len(mine)-1]
	}
	if sk.turn == nil {
		sk.turn = map[string]int{}
	}
	c := mine[sk.turn[appID]%len(mine)]
	sk.turn[appID]++
	return c
}

// ack records a client's acknowledgement. Messages that are not JSON, or that
// name an envelope the fake never sent, are ignored, and the first ack of an
// envelope stands.
func (sk *sockets) ack(msg []byte) {
	var res struct {
		EnvelopeID string          `json:"envelope_id"`
		Payload    json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(msg, &res) != nil {
		return
	}
	sk.settle(res.EnvelopeID, res.Payload)
}

// settle records the ack of the envelope id with payload, unless the envelope is one the fake never sent, or
// is acked already: the first ack stands. An app in HTTP mode acks with its answer's 200, and with no payload:
// the fake reads none from the answer's body.
func (sk *sockets) settle(id string, payload json.RawMessage) {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	env := sk.pending[id]
	if env == nil {
		return
	}
	select {
	case <-env.acked:
	default:
		env.payload = payload
		close(env.acked)
	}
}

// issueRequest records a new request to an app in HTTP mode, not yet acked and not yet sent for the last time (finish), and
// returns its ID.
func (sk *sockets) issueRequest() string {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	id := sk.issueLocked()
	sk.pending[id].sent = make(chan struct{})
	return id
}

// issueLocked records a new Socket Mode envelope, not yet acked, and returns its ID. The fake never sends one again, so it is
// sent for the last time as it is issued. The caller holds mu.
func (sk *sockets) issueLocked() string {
	sk.issued++
	id := fmt.Sprintf("env-%06d", sk.issued)
	sent := make(chan struct{})
	close(sent)
	sk.pending[id] = &envelope{acked: make(chan struct{}), sent: sent}
	return id
}

// finish records that the fake will send the request id no more. A second call is a no-op.
func (sk *sockets) finish(id string) {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	env := sk.pending[id]
	if env == nil {
		return
	}
	select {
	case <-env.sent:
	default:
		close(env.sent)
	}
}

// WaitDone blocks until the fake will send none of the envelopes again, or ctx ends, or the Server is closed: an envelope over
// Socket Mode at once, since the fake never sends one again, and a request to an app in HTTP mode once it is answered 200, or
// its retries have run out or ended (a 4xx, a revoked app, the Server closing). A test that counts what an app did for a press
// waits for it, so that a retry the app saw twice is counted. It fails at once for an envelope ID the fake never issued.
func (s *Server) WaitDone(ctx context.Context, envelopeIDs ...string) error {
	for _, id := range envelopeIDs {
		env := s.envelope(id)
		if env == nil {
			return fmt.Errorf("fakeslack: no envelope %q was sent: %w", id, ErrNotFound)
		}
		select {
		case <-env.sent:
		case <-ctx.Done():
			return fmt.Errorf("fakeslack: waiting for the last delivery of %s: %w", id, ctx.Err())
		case <-s.sock.closing:
			return fmt.Errorf("fakeslack: closed while waiting for the last delivery of %s", id)
		}
	}
	return nil
}

// envelope returns the record of an envelope the fake sent, or nil.
func (s *Server) envelope(id string) *envelope {
	s.sock.mu.Lock()
	defer s.sock.mu.Unlock()
	return s.sock.pending[id]
}

// Ack reports whether the client has acknowledged the envelope, and the payload
// it acked with, if any.
func (s *Server) Ack(envelopeID string) (payload json.RawMessage, acked bool) {
	env := s.envelope(envelopeID)
	if env == nil {
		return nil, false
	}
	select {
	case <-env.acked:
		return env.payload, true
	default:
		return nil, false
	}
}

// WaitAck blocks until the client has acknowledged every envelope, or ctx ends,
// or the Server is closed. It fails at once for an envelope ID the fake never
// issued.
func (s *Server) WaitAck(ctx context.Context, envelopeIDs ...string) error {
	for _, id := range envelopeIDs {
		env := s.envelope(id)
		if env == nil {
			return fmt.Errorf("fakeslack: no envelope %q was sent: %w", id, ErrNotFound)
		}
		s.sock.waiters.Add(1)
		defer s.sock.waiters.Add(-1)
		select {
		case <-env.acked:
		case <-ctx.Done():
			return fmt.Errorf("fakeslack: waiting for ack of %s: %w", id, ctx.Err())
		case <-s.sock.closing:
			return fmt.Errorf("fakeslack: closed while waiting for ack of %s", id)
		}
	}
	return nil
}
