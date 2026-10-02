package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// Slack drives the fake Slack through its /_control/ surface, so a scenario acts as Slack users
// and reads what courier posted. The surface has no authentication, so this client is only for
// tests inside the cluster, and the fake is never published.
type Slack struct {
	c     *client
	mu    sync.Mutex
	known map[string]bool // the users Know has told the fake of
}

// NewSlack returns a client for the fake Slack at baseURL.
func NewSlack(baseURL string) *Slack { return &Slack{c: newClient(baseURL), known: map[string]bool{}} }

// Mention posts text as user in channel. It must mention the bot, and a client must be connected
// over Socket Mode (else 409), because the fake delivers the events. The UserMessage lists the
// envelope of each event delivered; pass them to AckWait.
func (s *Slack) Mention(ctx context.Context, channel, user, text string) (fakeslack.UserMessage, error) {
	return s.say(ctx, "mention", map[string]string{"channel": channel, "user": user, "text": text})
}

// Reply posts text as user in the thread of the message at threadTS.
func (s *Slack) Reply(ctx context.Context, channel, threadTS, user, text string) (fakeslack.UserMessage, error) {
	return s.say(ctx, "reply", map[string]string{"channel": channel, "thread_ts": threadTS, "user": user, "text": text})
}

// DM posts text as user in their direct-message channel with the bot (fakeslack.DMChannel).
func (s *Slack) DM(ctx context.Context, user, text string) (fakeslack.UserMessage, error) {
	return s.say(ctx, "dm", map[string]string{"user": user, "text": text})
}

// say makes one of the three posts. If the fake stored the message but could not deliver its events
// (no client connected any more), the error body carries the stored message, and say returns it
// with the error, so a caller that retries need not post it twice.
func (s *Slack) say(ctx context.Context, action string, in map[string]string) (fakeslack.UserMessage, error) {
	var out fakeslack.UserMessage
	err := s.c.do(ctx, http.MethodPost, "/_control/"+action, in, &out)
	var se *StatusError
	if errors.As(err, &se) {
		var partial struct {
			Message fakeslack.UserMessage `json:"message"`
		}
		if json.Unmarshal([]byte(se.Body), &partial) == nil {
			out = partial.Message
		}
	}
	return out, err
}

// Click presses the button actionID, carrying value, on the message at messageTS as user. It
// returns the envelope of the interaction, for AckWait.
func (s *Slack) Click(ctx context.Context, user, messageTS, actionID, value string) (string, error) {
	return s.interact(ctx, "click", map[string]any{"user": user, "message_ts": messageTS, "action_id": actionID, "value": value})
}

// Submit presses the submit button actionID on the message at messageTS as user, with the answers
// they entered (block_id, then action_id, then the element's state). It returns the envelope.
func (s *Slack) Submit(ctx context.Context, user, messageTS, actionID string, values fakeslack.StateValues) (string, error) {
	return s.interact(ctx, "submit", map[string]any{"user": user, "message_ts": messageTS, "action_id": actionID, "values": values})
}

func (s *Slack) interact(ctx context.Context, action string, in map[string]any) (string, error) {
	var out struct {
		EnvelopeID string `json:"envelope_id"`
	}
	err := s.c.do(ctx, http.MethodPost, "/_control/"+action, in, &out)
	return out.EnvelopeID, err
}

// AddUser registers (or replaces) a workspace user, for users.info.
func (s *Slack) AddUser(ctx context.Context, u fakeslack.User) error {
	return s.c.do(ctx, http.MethodPost, "/_control/users", u, nil)
}

// Know registers user with the fake, once for this client, with a name made from the ID: a user that
// writes a message must be one the fake can answer users.info for, or courier logs a warning for the
// lookup. A later call for the same user does nothing; AddUser is for a user with more to say.
func (s *Slack) Know(ctx context.Context, user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.known[user] {
		return nil
	}
	if err := s.AddUser(ctx, fakeslack.User{ID: user, Name: strings.ToLower(user), RealName: "Scenario " + user}); err != nil {
		return err
	}
	s.known[user] = true
	return nil
}

// SetMembers registers channel and replaces its membership, for conversations.members.
func (s *Slack) SetMembers(ctx context.Context, channel string, users ...string) error {
	return s.c.do(ctx, http.MethodPost, "/_control/members", map[string]any{"channel": channel, "users": users}, nil)
}

// Connections returns how many Socket Mode clients are connected.
func (s *Slack) Connections(ctx context.Context) (int, error) {
	var out struct {
		Connections int `json:"connections"`
	}
	err := s.c.do(ctx, http.MethodGet, "/_control/connections", nil, &out)
	return out.Connections, err
}

// Messages returns everything stored in channel, in order.
func (s *Slack) Messages(ctx context.Context, channel string) ([]fakeslack.MessageView, error) {
	return s.read(ctx, "/_control/messages?"+url.Values{"channel": {channel}}.Encode())
}

// Thread returns the thread rooted at ts in channel, root first; an unknown thread is a 404.
func (s *Slack) Thread(ctx context.Context, channel, ts string) ([]fakeslack.MessageView, error) {
	return s.read(ctx, "/_control/thread?"+url.Values{"channel": {channel}, "ts": {ts}}.Encode())
}

func (s *Slack) read(ctx context.Context, path string) ([]fakeslack.MessageView, error) {
	var msgs []fakeslack.MessageView
	err := s.c.do(ctx, http.MethodGet, path, nil, &msgs)
	return msgs, err
}

// acked reports at once whether courier has acked the envelope, and the ack's payload. An
// unknown envelope is a 404.
func (s *Slack) acked(ctx context.Context, envelopeID string) (bool, json.RawMessage, error) {
	var out struct {
		Acked   bool            `json:"acked"`
		Payload json.RawMessage `json:"payload"`
	}
	err := s.c.do(ctx, http.MethodGet, "/_control/acks/"+url.PathEscape(envelopeID), nil, &out)
	return out.Acked, out.Payload, err
}

// AckWait waits up to timeout for courier to ack the envelope and returns the ack's payload. It
// polls acked rather than holding a request open.
func (s *Slack) AckWait(ctx context.Context, timeout time.Duration, envelopeID string) (json.RawMessage, error) {
	var payload json.RawMessage
	err := Wait(ctx, timeout, "envelope "+envelopeID+" to be acked", func(ctx context.Context) (bool, error) {
		acked, p, err := s.acked(ctx, envelopeID)
		payload = p
		return acked, err
	})
	return payload, err
}
