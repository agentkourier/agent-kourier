package fakeslack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoConnection is returned when an envelope is sent while no Socket Mode
// client is connected.
var ErrNoConnection = errors.New("fakeslack: no Socket Mode client is connected")

// envelope is the fake's record of one envelope it sent.
type envelope struct {
	acked   chan struct{} // closed on the first ack
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
// "interactive", "slash_commands", ...) carrying payload, and returns its
// envelope_id. It fails with ErrNoConnection if no client is connected, or if
// the write to the newest one fails.
//
// UNCONFIRMED(WP-04): that envelopes go to the newest connection only, where
// Slack spreads them over up to 10; and that the fake never redelivers an
// envelope that is not acked, where Slack retries with retry_attempt set.
func (s *Server) SendEnvelope(typ string, payload any, opts ...EnvelopeOption) (string, error) {
	sk := &s.sock
	sk.mu.Lock()
	if len(sk.conns) == 0 {
		sk.mu.Unlock()
		return "", ErrNoConnection
	}
	c := sk.conns[len(sk.conns)-1]
	sk.issued++
	id := fmt.Sprintf("env-%06d", sk.issued)
	sk.pending[id] = &envelope{acked: make(chan struct{})}
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
	sk.mu.Lock()
	defer sk.mu.Unlock()
	env := sk.pending[res.EnvelopeID]
	if env == nil {
		return
	}
	select {
	case <-env.acked:
	default:
		env.payload = res.Payload
		close(env.acked)
	}
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
