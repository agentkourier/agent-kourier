package fakeslack

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// UserMessage is a message a test posted as a user: where it was stored, and
// the envelope_id of each event delivered for it. Pass Envelopes to WaitAck.
type UserMessage struct {
	Channel   string   `json:"channel"`
	TS        string   `json:"ts"`
	Envelopes []string `json:"envelopes"`
	// AppEnvelopes is the same envelope IDs by the AppID of the app they went to, in order. It is set only on a
	// Server that hosts more than one app, and leaves out an app that got none: one that is not in the channel,
	// or that has no client connected.
	AppEnvelopes map[string][]string `json:"app_envelopes,omitempty"`
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
// default, since Agent Kourier reads the mention out of the text. Slack sends no
// app_mention in a direct message, so a D channel is refused: use DM.
//
// With several apps (WithApp) the text may mention any of their bots. The
// app_mention goes to the apps mentioned that are in the channel, and the
// message event to every app in it (see SetMembers).
//
// UNCONFIRMED(WP-04): the order of the two events; that both are sent, as
// they are for an app subscribed to app_mention and message.channels; that a
// DM gets no app_mention; and the fields of each event, which are the ones
// slack-go's types carry that Agent Kourier needs (no client_msg_id, blocks,
// event_context or token).
func (s *Server) Mention(channel, user, text string) (UserMessage, error) {
	if len(s.mentionedApps(text)) == 0 {
		return UserMessage{}, invalid("fakeslack: text %q does not mention the bot %s", text, s.botMentions())
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

// botMentions names the mention each app's bot answers to, for an error.
func (s *Server) botMentions() string {
	var names []string
	for _, a := range s.apps {
		names = append(names, "<@"+a.BotUser+">")
	}
	return strings.Join(names, " or ")
}

// DM posts text as user in their direct-message channel with the default app's
// bot, DMChannel of user, and delivers a message event to that app. Reply
// threads in it. DMApp names the app.
func (s *Server) DM(user, text string) (UserMessage, error) {
	return s.DMApp(s.defaultApp().AppID, user, text)
}

// DMApp is DM in the direct-message channel between user and the bot of the app
// appID, DMChannelFor appID and user, whose events go to that app alone. It fails
// with ErrNotFound for an app the Server does not host.
func (s *Server) DMApp(appID, user, text string) (UserMessage, error) {
	a, err := s.appNamed(appID)
	if err != nil {
		return UserMessage{}, err
	}
	channel := s.DMChannelFor(a.AppID, user)
	s.mu.Lock()
	s.dmOwner[channel] = a.AppID
	s.mu.Unlock()
	return s.userMessage(channel, "", user, text)
}

// DMChannel returns the ID of the direct-message channel between user and the
// default app's bot. It starts with D, as Slack's do.
func DMChannel(user string) string { return "D" + user }

// DMChannelFor returns the ID of the direct-message channel between user and the
// bot of the app appID: DMChannel for the default app, and a channel of its own
// for any other, DMChannel with the app ID appended, since two bots have two
// direct-message channels with a user.
func (s *Server) DMChannelFor(appID, user string) string {
	if appID == s.defaultApp().AppID {
		return DMChannel(user)
	}
	return DMChannel(user) + appID
}

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
// preceded by an app_mention if it mentions a bot outside a direct message. The
// events go to the apps that are in the channel (recipients), each over its own
// connection, and an app_mention only to the apps mentioned.
// Nothing happens if no app that would get the events has a client connected.
// If the clients leave after that check, the message stays stored and the error
// is ErrNoConnection: the event is lost, as it would be in Slack. An event that
// reaches some apps and not others is lost for the others without an error, and
// the error is returned only if no envelope went out at all.
func (s *Server) userMessage(channel, threadTS, user, text string) (UserMessage, error) {
	if channel == "" || user == "" || text == "" {
		return UserMessage{}, invalid("fakeslack: a user message needs a channel, a user and text")
	}
	recipients := s.recipients(channel)
	if !s.anyConnected(recipients) {
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
	mentioned := onlyApps(recipients, s.mentionedApps(text))
	eventTypes := []string{"message"}
	if chType != "im" && len(mentioned) > 0 {
		eventTypes = []string{"app_mention", "message"}
	}
	var failed error
	for _, typ := range eventTypes {
		ev := map[string]any{"type": typ, "user": user, "text": text, "ts": m.TS, "channel": channel, "event_ts": m.TS}
		if typ == "message" {
			ev["channel_type"] = chType
		}
		if threadTS != "" {
			ev["thread_ts"] = threadTS
		}
		targets := recipients
		if typ == "app_mention" {
			targets = mentioned
		}
		ids, err := s.deliver(targets, ev)
		out.addEnvelopes(s, ids)
		if err != nil && failed == nil {
			failed = err
		}
	}
	// Each event goes out whatever became of the one before: an app_mention for an app that has left must not hold
	// back the message event the others are waiting for. Only an event nobody got is an error.
	if len(out.Envelopes) == 0 {
		return out, failed
	}
	return out, nil
}

// addEnvelopes records the envelopes of one event, in the order of the Server's apps.
func (m *UserMessage) addEnvelopes(s *Server, ids map[string]string) {
	for _, a := range s.apps {
		id, ok := ids[a.AppID]
		if !ok {
			continue
		}
		m.Envelopes = append(m.Envelopes, id)
		if len(s.apps) > 1 {
			if m.AppEnvelopes == nil {
				m.AppEnvelopes = map[string][]string{}
			}
			m.AppEnvelopes[a.AppID] = append(m.AppEnvelopes[a.AppID], id)
		}
	}
}

// anyConnected says whether any of apps has a Socket Mode client connected.
func (s *Server) anyConnected(apps []App) bool {
	s.sock.mu.Lock()
	defer s.sock.mu.Unlock()
	return slices.ContainsFunc(apps, func(a App) bool { return s.sock.countLocked(a.AppID) > 0 })
}

// deliver sends event to each of apps, as an events_api envelope made for that
// app, and returns the envelope_id each got by AppID. An app with no client
// connected, or whose write fails, gets nothing, as Slack drops events for an app
// that is not listening; the error is ErrNoConnection, wrapped, only when no app
// got the event.
func (s *Server) deliver(apps []App, event map[string]any) (map[string]string, error) {
	got := map[string]string{}
	var failed error
	for _, a := range apps {
		id, err := s.eventCallbackTo(a, event)
		if err != nil {
			if failed == nil {
				failed = err
			}
			continue
		}
		got[a.AppID] = id
	}
	if len(got) == 0 {
		if failed == nil {
			failed = ErrNoConnection
		}
		return nil, failed
	}
	return got, nil
}

// eventCallbackTo wraps an Events API event in its event_callback payload, with
// the app's own team, api_app_id and authorizations, and sends it to that app
// as an events_api envelope.
func (s *Server) eventCallbackTo(app App, event map[string]any) (string, error) {
	s.mu.Lock()
	s.events++
	eventID, eventTime := fmt.Sprintf("Ev%08d", s.events), s.now().Unix()
	s.mu.Unlock()
	return s.sendEnvelope(app.AppID, "events_api", map[string]any{
		"type": "event_callback", "team_id": app.TeamID, "api_app_id": app.AppID,
		"event": event, "event_id": eventID, "event_time": eventTime,
		"authorizations": []map[string]any{{"team_id": app.TeamID, "user_id": app.BotUser, "is_bot": true}},
	})
}
