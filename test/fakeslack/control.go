package fakeslack

import (
	"fmt"
	"strings"
)

// teamID is the workspace every event and interaction claims to come from.
const teamID = "TFAKE"

// UserMessage is a message a test posted as a user: where it was stored, and
// the envelope_id of each event delivered for it. Pass Envelopes to WaitAck.
type UserMessage struct {
	Channel, TS string
	Envelopes   []string
}

// Mention posts text in channel as user and delivers an app_mention event and
// a message event for it. The text must mention the bot, as <@UFAKEBOT> by
// default, since courier reads the mention out of the text.
//
// UNCONFIRMED(WP-04): the order of the two events; that both are sent, as
// they are for an app subscribed to app_mention and message.channels; and the
// fields of each event, which are the ones slack-go's types carry that courier
// needs (no client_msg_id, blocks, event_context or token).
func (s *Server) Mention(channel, user, text string) (UserMessage, error) {
	if !strings.Contains(text, "<@"+s.bot.user+">") {
		return UserMessage{}, fmt.Errorf("fakeslack: text %q does not mention the bot <@%s>", text, s.bot.user)
	}
	return s.userMessage(channel, "", user, text, "app_mention", "message")
}

// Reply posts text in the thread of a message in channel as user and delivers
// a message event. The thread must exist.
//
// UNCONFIRMED(WP-04): that a reply mentioning the bot gets no app_mention
// event here, where Slack sends one.
func (s *Server) Reply(channel, threadTS, user, text string) (UserMessage, error) {
	return s.userMessage(channel, threadTS, user, text, "message")
}

// DM posts text as user in their direct-message channel with the bot, DMChannel
// of user, and delivers a message event. Reply threads in it.
func (s *Server) DM(user, text string) (UserMessage, error) {
	return s.userMessage(DMChannel(user), "", user, text, "message")
}

// DMChannel returns the ID of the direct-message channel between user and the
// bot. It starts with D, as Slack's do.
func DMChannel(user string) string { return "D" + user }

// channelType returns the channel_type a message event reports for a channel.
func channelType(channel string) string {
	if strings.HasPrefix(channel, "D") {
		return "im"
	}
	return "channel"
}

// userMessage stores a message from user and delivers one event of each type.
// It does nothing if no client is connected to receive them.
func (s *Server) userMessage(channel, threadTS, user, text string, eventTypes ...string) (UserMessage, error) {
	if s.Connections() == 0 {
		return UserMessage{}, ErrNoConnection
	}
	m := &Message{Type: "message", User: user, Text: text, ThreadTS: threadTS}
	s.mu.Lock()
	code := s.addLocked(channel, m)
	s.mu.Unlock()
	if code != "" {
		return UserMessage{}, fmt.Errorf("fakeslack: posting as %s in %s: %s", user, channel, code)
	}

	out := UserMessage{Channel: channel, TS: m.TS}
	for _, typ := range eventTypes {
		ev := map[string]any{"type": typ, "user": user, "text": text, "ts": m.TS, "channel": channel, "event_ts": m.TS}
		if typ == "message" {
			ev["channel_type"] = channelType(channel)
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
		"type": "event_callback", "team_id": teamID, "api_app_id": s.bot.app,
		"event": event, "event_id": eventID, "event_time": eventTime,
		"authorizations": []map[string]any{{"team_id": teamID, "user_id": s.bot.user, "is_bot": true}},
	})
}
