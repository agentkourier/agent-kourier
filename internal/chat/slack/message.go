package slack

import (
	"context"
	"strings"

	"github.com/slack-go/slack/slackevents"
)

// Message is a chat message that may be a turn: from a person, with text, in a channel, or from a bot that a
// trigger names (Bot).
type Message struct {
	Channel  string
	TS       string // the message's own ts
	ThreadTS string // the thread it is in; empty outside any thread
	User     string // empty for a message of an incoming webhook, which has no user
	Text     string // verbatim, with any mention of the bot in it
	// ChannelType is "channel", "group" (private) and so on; empty on an app_mention, which has none.
	ChannelType string
	Mentioned   bool // the message mentions the bot; always true for an app_mention, and never for a bot's message
	// Bot is set if a bot posted the message.
	Bot BotPost
}

// BotPost is what a bot's message adds to a person's. The text fields are the first legacy attachment's, raw:
// whatever mrkdwn_in says about how Slack renders them, they are the characters Slack delivered, neither
// rendered nor unescaped. A message with several attachments is read from its first alone, so that the title,
// the link and the fallback a trigger matches are one attachment's and not a mix; the others are not read.
type BotPost struct {
	// ID is the message's bot_id. Empty means the message is a person's.
	ID string
	// Title, TitleLink, Fallback and Footer are the attachment's title, title_link, fallback and footer,
	// and Text is its text.
	Title, TitleLink, Fallback, Footer, Text string
}

// content says whether the post has anything for a trigger to read.
func (b BotPost) content() bool {
	return b.Title != "" || b.TitleLink != "" || b.Fallback != "" || b.Footer != "" || b.Text != ""
}

// Sink receives what Run decodes: messages, and presses of buttons on messages.
type Sink interface {
	// Interaction takes i. An error leaves the event unacknowledged, as for Message, which is
	// the person's only sign that the press failed. UNCONFIRMED(WP-04): Slack probably does not
	// send an interactive payload again, so nothing may rely on a failed press being redelivered.
	Interaction(ctx context.Context, i Interaction) error
	// Message takes m. An error means it was not taken: Slack sends the event again. A
	// mention arrives twice (app_mention and message), with the same Channel and TS.
	Message(ctx context.Context, m Message) error
}

// BotFilter is an optional interface of a Sink: whether it takes the messages of a bot, asked per channel. A
// Sink without it is given no bot's message, so that Run fails closed.
type BotFilter interface {
	// AcceptsBot says whether the Sink takes messages of the bot botID in channel.
	AcceptsBot(channel, botID string) bool
}

// keptSubtype reports whether a message event of that subtype is a person's message: a plain
// one, a thread reply also sent to the channel, or one with a file (its text only).
// UNCONFIRMED(WP-04): that these are all the subtypes of a person's messages.
func keptSubtype(s string) bool {
	return s == "" || s == "thread_broadcast" || s == "file_share"
}

// botSubtype reports whether a message event that has a bot_id and that subtype is a bot's post: an incoming
// webhook's (bot_message), or an app's own post (no subtype, with a user and an app_id too). A bot's file
// share or thread broadcast is neither.
// UNCONFIRMED(WP-04): that these are all the subtypes of a bot's posts that matter here.
func botSubtype(s string) bool {
	return s == "" || s == "bot_message"
}

// self is who courier is on the workspace, from auth.test: the user ID its posts carry, and the bot ID.
type self struct{ user, bot string }

// verdict is what decode makes of an event.
type verdict int

const (
	// dropped is an event that is no message for courier: it is acknowledged and nothing more.
	dropped verdict = iota
	// forward is a message for the Sink: a person's, or a bot's that is not courier's own.
	forward
	// ownPost is a post of courier itself. It is never forwarded, but Run tells a trigger that names
	// courier's bot ID from one that does not (Client.refuseOwn).
	ownPost
)

// decode returns the Message that inner carries, and what to do with it (spec 8.2, D36).
//   - A person's message is forwarded if it is plain, a thread_broadcast or a file_share, with a user, a
//     channel, a ts and text.
//   - A bot's message (a message event with a bot_id and no subtype or bot_message) is forwarded with its
//     content, for Run to ask the Sink whether a trigger names the bot. A message of courier's own, by user ID
//     or by bot ID, is never forwarded (ownPost): two bots would answer each other without end. A bot's message
//     of another subtype, one with no content, and an app_mention by a bot are dropped.
//   - Edits, deletions and other subtypes, whoever they are of, are dropped: a bot's edit is no new alert.
//   - Direct messages are dropped (Phase 4), and anything lacking a channel or ts.
func decode(me self, inner any) (Message, verdict) {
	switch e := inner.(type) {
	case *slackevents.AppMentionEvent:
		// An edit that adds a mention arrives as an app_mention with edited set; edits are ignored.
		// UNCONFIRMED(WP-04): that Slack sends it so.
		if e.Edited != nil || e.BotID != "" || e.User == me.user || e.User == "" || e.Channel == "" || e.TimeStamp == "" || e.Text == "" {
			return Message{}, dropped
		}
		return Message{Channel: e.Channel, TS: e.TimeStamp, ThreadTS: e.ThreadTimeStamp, User: e.User, Text: e.Text, Mentioned: true}, forward
	case *slackevents.MessageEvent:
		if e.IsIM() {
			return Message{}, dropped
		}
		m := Message{Channel: e.Channel, TS: e.TimeStamp, ThreadTS: e.ThreadTimeStamp, User: e.User, Text: e.Text, ChannelType: e.ChannelType}
		if m.Channel == "" || m.TS == "" {
			return Message{}, dropped
		}
		isOwn := (me.user != "" && e.User == me.user) || (me.bot != "" && e.BotID == me.bot)
		switch {
		case e.BotID != "" && botSubtype(e.SubType):
			m.Bot = botPost(e)
			switch {
			case isOwn:
				return m, ownPost
			case !m.Bot.content() && m.Text == "":
				return Message{}, dropped
			}
			return m, forward // Mentioned stays false: a bot's mention starts nothing
		case !keptSubtype(e.SubType) || e.BotID != "" || isOwn || m.User == "" || m.Text == "":
			return Message{}, dropped
		}
		m.Mentioned = strings.Contains(e.Text, "<@"+me.user+">")
		return m, forward
	}
	return Message{}, dropped
}

// botPost flattens e's first legacy attachment into the fields a trigger reads. A message with no
// attachment, as an app's own post is, has only its bot ID, and its text is the message's.
func botPost(e *slackevents.MessageEvent) BotPost {
	b := BotPost{ID: e.BotID}
	if e.Message == nil || len(e.Message.Attachments) == 0 {
		return b
	}
	a := e.Message.Attachments[0]
	b.Title, b.TitleLink, b.Fallback, b.Footer, b.Text = a.Title, a.TitleLink, a.Fallback, a.Footer, a.Text
	return b
}
