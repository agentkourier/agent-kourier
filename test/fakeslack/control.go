package fakeslack

import (
	"errors"
	"fmt"
	"strings"
)

// UserMessage is a message a test posted as a user: where it was stored, and
// the envelope_id of each event delivered for it. Pass Envelopes to WaitAck.
type UserMessage struct {
	Channel   string   `json:"channel"`
	TS        string   `json:"ts"`
	Envelopes []string `json:"envelopes"`
}

// ErrNotFound is wrapped by the errors for a message, thread, or action that
// does not exist.
var ErrNotFound = errors.New("fakeslack: not found")

// ErrInvalid is matched by errors.Is for a request the fake refuses because no
// user or Slack app could have made it: missing fields, a mention that does not
// mention the bot, an answer the message's elements cannot take.
var ErrInvalid = errors.New("fakeslack: invalid request")

type invalidError string

func (e invalidError) Error() string      { return string(e) }
func (invalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(format string, args ...any) error { return invalidError(fmt.Sprintf(format, args...)) }

// Mention posts text in a channel as user and delivers an app_mention event and
// a message event for it. The text must mention the bot, as <@UFAKEBOT> by
// default, since courier reads the mention out of the text. Slack sends no
// app_mention in a direct message, so a D channel is refused: use DM.
//
// UNCONFIRMED(WP-04): the order of the two events; that both are sent, as
// they are for an app subscribed to app_mention and message.channels; that a
// DM gets no app_mention; and the fields of each event, which are the ones
// slack-go's types carry that courier needs (no client_msg_id, blocks,
// event_context or token).
func (s *Server) Mention(channel, user, text string) (UserMessage, error) {
	if !s.mentionsBot(text) {
		return UserMessage{}, invalid("fakeslack: text %q does not mention the bot <@%s>", text, s.bot.user)
	}
	if s.channelType(channel) == "im" {
		return UserMessage{}, invalid("fakeslack: %s is a direct message, where Slack sends no app_mention; use DM", channel)
	}
	return s.userMessage(channel, "", user, text)
}

// Reply posts text in the thread of a message in channel as user and delivers
// a message event; the thread must exist, and threadTS names it. If the text
// mentions the bot, in a channel that is not a direct message, an app_mention
// event comes first, with the thread_ts, as for a Mention.
func (s *Server) Reply(channel, threadTS, user, text string) (UserMessage, error) {
	if threadTS == "" {
		return UserMessage{}, invalid("fakeslack: a reply needs a thread_ts")
	}
	return s.userMessage(channel, threadTS, user, text)
}

func (s *Server) mentionsBot(text string) bool { return strings.Contains(text, "<@"+s.bot.user+">") }

// DM posts text as user in their direct-message channel with the bot, DMChannel
// of user, and delivers a message event. Reply threads in it.
func (s *Server) DM(user, text string) (UserMessage, error) {
	return s.userMessage(DMChannel(user), "", user, text)
}

// DMChannel returns the ID of the direct-message channel between user and the
// bot. It starts with D, as Slack's do.
func DMChannel(user string) string { return "D" + user }

// SetChannelType sets the channel_type that message events report for a
// channel: "group" for a private channel, say. Channels default to "channel",
// and to "im" if their ID starts with D.
//
// UNCONFIRMED(WP-04): those defaults; Slack's private channels are "group".
func (s *Server) SetChannelType(channel, channelType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channelTypes[channel] = channelType
}

// channelType returns the channel_type a message event reports for a channel.
func (s *Server) channelType(channel string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch t := s.channelTypes[channel]; {
	case t != "":
		return t
	case strings.HasPrefix(channel, "D"):
		return "im"
	}
	return "channel"
}

// userMessage stores a message from user and delivers a message event for it,
// preceded by an app_mention if it mentions the bot outside a direct message.
// Nothing happens if no client is connected to receive the events. If the client
// leaves after that check, the message stays stored and the error is
// ErrNoConnection: the event is lost, as it would be in Slack.
func (s *Server) userMessage(channel, threadTS, user, text string) (UserMessage, error) {
	if channel == "" || user == "" {
		return UserMessage{}, invalid("fakeslack: a user message needs a channel and a user")
	}
	if s.Connections() == 0 {
		return UserMessage{}, ErrNoConnection
	}
	m := &Message{Type: "message", User: user, Text: text, ThreadTS: threadTS}
	s.mu.Lock()
	code := s.addLocked(channel, m)
	s.mu.Unlock()
	switch code {
	case "":
	case "thread_not_found":
		return UserMessage{}, fmt.Errorf("fakeslack: posting as %s in %s: %w", user, channel, ErrNotFound)
	default:
		return UserMessage{}, invalid("fakeslack: posting as %s in %s: %s", user, channel, code)
	}

	out := UserMessage{Channel: channel, TS: m.TS}
	chType := s.channelType(channel)
	eventTypes := []string{"message"}
	if chType != "im" && s.mentionsBot(text) {
		eventTypes = []string{"app_mention", "message"}
	}
	for _, typ := range eventTypes {
		ev := map[string]any{"type": typ, "user": user, "text": text, "ts": m.TS, "channel": channel, "event_ts": m.TS}
		if typ == "message" {
			ev["channel_type"] = chType
		}
		if threadTS != "" {
			ev["thread_ts"] = threadTS
		}
		id, err := s.eventCallback(ev)
		if err != nil {
			return out, err
		}
		out.Envelopes = append(out.Envelopes, id)
	}
	return out, nil
}

// eventCallback wraps an Events API event in its event_callback payload and
// sends it as an events_api envelope.
func (s *Server) eventCallback(event map[string]any) (string, error) {
	s.mu.Lock()
	s.events++
	eventID, eventTime := fmt.Sprintf("Ev%08d", s.events), s.now().Unix()
	s.mu.Unlock()
	return s.SendEnvelope("events_api", map[string]any{
		"type": "event_callback", "team_id": s.team.id, "api_app_id": s.bot.app,
		"event": event, "event_id": eventID, "event_time": eventTime,
		"authorizations": []map[string]any{{"team_id": s.team.id, "user_id": s.bot.user, "is_bot": true}},
	})
}
