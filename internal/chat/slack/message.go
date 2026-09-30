package slack

import (
	"context"
	"strings"

	"github.com/slack-go/slack/slackevents"
)

// Message is a chat message that may be a turn: from a person, with text, in a channel.
type Message struct {
	Channel  string
	TS       string // the message's own ts
	ThreadTS string // the thread it is in; empty outside any thread
	User     string
	Text     string // verbatim, with any mention of the bot in it
	// ChannelType is "channel", "group" (private) and so on; empty on an app_mention, which has none.
	ChannelType string
	Mentioned   bool // the message mentions the bot; always true for an app_mention
}

// Sink receives the messages Run decodes.
type Sink interface {
	// Message takes m. An error means it was not taken: Slack sends the event again. A
	// mention arrives twice (app_mention and message), with the same Channel and TS.
	Message(ctx context.Context, m Message) error
}

// keptSubtype reports whether a message event of that subtype is a person's message: a plain
// one, a thread reply also sent to the channel, or one with a file (its text only).
// UNCONFIRMED(WP-04): that these are all the subtypes of a person's messages.
func keptSubtype(s string) bool {
	return s == "" || s == "thread_broadcast" || s == "file_share"
}

// decode returns the Message that inner carries, and whether it may be a turn (spec 8.2). It
// drops courier's own messages (by user ID, for posts that arrive without a bot_id), every
// other bot's (two bots would answer each other without end; a bot's text is untrusted),
// edits, deletions, other subtypes, direct messages (Phase 4), and anything lacking a user,
// channel, ts or text.
func decode(botUser string, inner any) (Message, bool) {
	var m Message
	var botID string
	switch e := inner.(type) {
	case *slackevents.AppMentionEvent:
		m = Message{Channel: e.Channel, TS: e.TimeStamp, ThreadTS: e.ThreadTimeStamp, User: e.User, Text: e.Text, Mentioned: true}
		botID = e.BotID
	case *slackevents.MessageEvent:
		if !keptSubtype(e.SubType) || e.IsIM() {
			return Message{}, false
		}
		m = Message{Channel: e.Channel, TS: e.TimeStamp, ThreadTS: e.ThreadTimeStamp, User: e.User, Text: e.Text,
			ChannelType: e.ChannelType, Mentioned: strings.Contains(e.Text, "<@"+botUser+">")}
		botID = e.BotID
	default:
		return Message{}, false
	}
	if botID != "" || m.User == botUser || m.User == "" || m.Channel == "" || m.TS == "" || m.Text == "" {
		return Message{}, false
	}
	return m, true
}
