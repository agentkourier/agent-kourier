package slack

import (
	"cmp"
	"context"
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode"

	slackgo "github.com/slack-go/slack"
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
	// Mentioned says the message mentions the bot: always for an app_mention, whoever posted it. A bot's message event is
	// never Mentioned, whatever its text says: its mention arrives as an app_mention of its own.
	Mentioned bool
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

// BotMentionFilter is an optional interface of a Sink: whether it takes a bot's mention of Agent Kourier, asked per channel.
// A Sink without it is given none, so that Run fails closed.
type BotMentionFilter interface {
	// AcceptsBotMention says whether the Sink takes a mention by the bot botID in channel.
	AcceptsBotMention(channel, botID string) bool
}

// userToken is a user mention as Slack writes it into a message's text: <@UID>, or <@UID|label>. The label is
// whatever lies up to the first ">", a "|" included. Slack escapes "<" and ">" in what people type, so a raw token
// is always Slack's own, whether or not it stands in a quote or in code; neither character is allowed inside one, so
// that an unfinished token cannot swallow the one after it. The pattern is compiled once for every user:
// group 1 is the ID, and mentionedIn compares it whole, so that U1 is not found in <@U12>.
var userToken = regexp.MustCompile(`<@([^<>|]+)(?:\|[^<>]*)?>`)

// slackToken is a user, channel or special token in a message's text, and slackLink a link, with its label if it has one.
var (
	slackToken = regexp.MustCompile(`<[@#!][^<>]*>`)
	slackLink  = regexp.MustCompile(`<(?:[^<>|]*\|)?([^<>]*)>`)
)

// self is who Agent Kourier is on the workspace, from auth.test: the user ID its posts carry, and the bot ID.
type self struct{ user, bot string }

// verdict is what decode makes of an event.
type verdict int

const (
	// dropped is an event that is no message for Agent Kourier: it is acknowledged and nothing more.
	dropped verdict = iota
	// forward is a message for the Sink: a person's, or a bot's that is not Agent Kourier's own.
	forward
	// ownPost is a post of Agent Kourier itself. It is never forwarded, but Run tells a trigger that names
	// Agent Kourier's bot ID from one that does not (Client.refuseOwn).
	ownPost
)

// decode returns the Message that inner carries, and what to do with it (spec 8.2, D36).
//   - A person's message is forwarded if it is plain, a thread_broadcast or a file_share, with a user, a
//     channel, a ts and text. It is Mentioned if its text holds Agent Kourier's user token (mentionedIn), or its blocks
//     mention Agent Kourier where Slack registers a mention (mentionedInBlocks). A Slackbot post with no bot ID is no person's,
//     and is dropped (slackbotUser).
//   - A bot's message (a message event with a bot_id and no subtype or bot_message) is forwarded with its
//     content, for Run to ask the Sink whether a trigger names the bot. A message of Agent Kourier's own, by user ID
//     or by bot ID, is never forwarded (ownPost): two bots would answer each other without end. A bot's message
//     of another subtype and one with no content are dropped.
//   - A bot's mention is its app_mention, which is Mentioned, with what the post holds (mentionPost): Slack sends it where a
//     mention registers, whether the token is in the text or only in the blocks, so it is what says so
//     (docs/spikes/slack-mention-probe.md). It is forwarded for Run to ask the Sink, and Agent Kourier's own is an ownPost. Its
//     message event is the bot's message above, and never Mentioned.
//   - Edits, deletions and other subtypes, whoever they are of, are dropped: a bot's edit is no new alert.
//   - Direct messages are dropped (Phase 4), and anything lacking a channel or ts.
func decode(me self, inner any) (Message, verdict) {
	switch e := inner.(type) {
	case *slackevents.AppMentionEvent:
		// An edit that adds a mention arrives as an app_mention with edited set; edits are ignored.
		if e.Edited != nil || e.Channel == "" || e.TimeStamp == "" {
			return Message{}, dropped
		}
		if e.BotID != "" {
			m := Message{Channel: e.Channel, TS: e.TimeStamp, ThreadTS: e.ThreadTimeStamp, User: e.User, Text: e.Text, Mentioned: true, Bot: mentionPost(e)}
			if (me.user != "" && e.User == me.user) || (me.bot != "" && e.BotID == me.bot) {
				return m, ownPost
			}
			return m, forward
		}
		if !me.person(e.User) || e.Text == "" {
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
			return m, forward // Mentioned stays false: a bot's mention arrives as its own app_mention event
		case !keptSubtype(e.SubType) || e.BotID != "" || isOwn || !me.person(m.User) || m.Text == "":
			return Message{}, dropped
		}
		m.Mentioned = me.mentionedIn(e.Text) || me.mentionedInBlocks(blocksOf(e))
		return m, forward
	}
	return Message{}, dropped
}

// mentionedIn reports whether text holds Agent Kourier's user token, <@UID> or <@UID|label>, anywhere in it: Slack sends
// an app_mention for a token in a quote or in code, so the message event of that text counts as one too. Plain
// "@agent_kourier", another user's token, <!here>, <!channel> and a user group are not mentions.
func (me self) mentionedIn(text string) bool {
	if !strings.Contains(text, "<@") { // most text has none: skip the scan
		return false
	}
	for _, g := range userToken.FindAllStringSubmatch(text, -1) {
		if g[1] == me.user {
			return true
		}
	}
	return false
}

// slackbotUser is the user ID Slack gives Slackbot in every workspace. A Slackbot post that comes with no bot ID would otherwise be
// a person's, and Slackbot is no person. Its reminders do carry a bot ID (B01), so decode takes them, and a reminder's app_mention,
// as a bot's before it asks this (docs/spikes/slack-user-token-and-slackbot-probe.md). So does a post an app makes with a person's
// user token: Slack sends it with the app's bot ID.
const slackbotUser = "USLACKBOT"

// person says whether a post with no bot ID under user is a person's: it has a user, and the user is neither Agent Kourier nor
// Slackbot. A message event and an app_mention both ask it, so that neither forwards what the other drops.
func (me self) person(user string) bool {
	return user != "" && user != me.user && user != slackbotUser
}

// blocksOf is the blocks of a message event: its own, else those of the message it carries, as a history message's does.
func blocksOf(e *slackevents.MessageEvent) slackgo.Blocks {
	if len(e.Blocks.BlockSet) == 0 && e.Message != nil {
		return e.Message.Blocks
	}
	return e.Blocks
}

// mentionedInBlocks reports whether a message's blocks mention Agent Kourier where Slack registers a mention, and sends an
// app_mention for it, though the text may not hold the token: a rich_text block's user element of Agent Kourier, or its token
// in the text of a section or context block (docs/spikes/slack-mention-probe.md). It makes a person's message event agree with
// its app_mention, which may come before or after it.
func (me self) mentionedInBlocks(blocks slackgo.Blocks) bool {
	for _, block := range blocks.BlockSet {
		switch b := block.(type) {
		case *slackgo.SectionBlock:
			if b.Text != nil && me.mentionedIn(b.Text.Text) {
				return true
			}
			for _, f := range b.Fields {
				if f != nil && me.mentionedIn(f.Text) {
					return true
				}
			}
		case *slackgo.ContextBlock:
			for _, el := range b.ContextElements.Elements {
				if t, ok := el.(*slackgo.TextBlockObject); ok && me.mentionedIn(t.Text) {
					return true
				}
			}
		case *slackgo.RichTextBlock:
			for _, el := range b.Elements {
				if me.mentionedInRichText(el) {
					return true
				}
			}
		}
	}
	return false
}

// mentionedInRichText reports whether a rich_text element holds a user element of Agent Kourier, in a section, quote,
// preformatted text or list.
func (me self) mentionedInRichText(el slackgo.RichTextElement) bool {
	var inline []slackgo.RichTextSectionElement
	switch e := el.(type) {
	case *slackgo.RichTextSection:
		inline = e.Elements
	case *slackgo.RichTextQuote:
		inline = e.Elements
	case *slackgo.RichTextPreformatted:
		inline = e.Elements
	case *slackgo.RichTextList:
		return slices.ContainsFunc(e.Elements, me.mentionedInRichText)
	}
	return me.user != "" && slices.ContainsFunc(inline, func(p slackgo.RichTextSectionElement) bool {
		u, ok := p.(*slackgo.RichTextSectionUserElement)
		return ok && u.UserID == me.user
	})
}

// botPost flattens e's first legacy attachment into the fields a trigger reads. A message with no
// attachment, as an app's own post is, has only its bot ID, and its text is the message's.
func botPost(e *slackevents.MessageEvent) BotPost {
	if e.Message == nil {
		return BotPost{ID: e.BotID}
	}
	return postOf(e.BotID, e.Message.Attachments)
}

// postOf is the BotPost of a bot's message with these attachments: the first's fields.
func postOf(id string, attachments []slackgo.Attachment) BotPost {
	b := BotPost{ID: id}
	if len(attachments) == 0 {
		return b
	}
	a := attachments[0]
	b.Title, b.TitleLink, b.Fallback, b.Footer, b.Text = a.Title, a.TitleLink, a.Fallback, a.Footer, a.Text
	return b
}

// mentionPost is the BotPost of a bot's app_mention. Top-level text can be a mere fallback, whether the token registered from a
// section or a context block or the words are in a markdown block, so when the attachment has no text of its own, what the blocks
// say past the text is the post's body: no trigger's regex reads a mention, so this changes what no bot message trigger matches.
func mentionPost(e *slackevents.AppMentionEvent) BotPost {
	b := postOf(e.BotID, e.Attachments)
	if words := blockWords(e.Blocks); b.Text == "" && saysMore(e.Text, words) {
		b.Text = words
	}
	return b
}

// blockWords is the words of a post's blocks, a line for each piece: its section, context and header blocks, and its rich_text and
// markdown blocks, a markdown block being what Slack stores as rich_text with the post's text left as the fallback it was. People,
// channels and emoji in rich_text are left out: they are no words of a report, and text holds them if it needs them.
func blockWords(blocks slackgo.Blocks) string {
	var lines []string
	add := func(t *slackgo.TextBlockObject) {
		if t != nil && t.Text != "" {
			lines = append(lines, t.Text)
		}
	}
	for _, block := range blocks.BlockSet {
		switch b := block.(type) {
		case *slackgo.SectionBlock:
			add(b.Text)
			for _, f := range b.Fields {
				add(f)
			}
		case *slackgo.ContextBlock:
			for _, el := range b.ContextElements.Elements {
				if t, ok := el.(*slackgo.TextBlockObject); ok {
					add(t)
				}
			}
		case *slackgo.HeaderBlock:
			add(b.Text)
		case *slackgo.MarkdownBlock:
			if b.Text != "" {
				lines = append(lines, b.Text)
			}
		case *slackgo.RichTextBlock:
			for _, el := range b.Elements {
				lines = append(lines, richTextLines(el)...)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// richTextLines is the words of a rich_text element, a line for each section of it.
func richTextLines(el slackgo.RichTextElement) []string {
	var inline []slackgo.RichTextSectionElement
	switch e := el.(type) {
	case *slackgo.RichTextSection:
		inline = e.Elements
	case *slackgo.RichTextQuote:
		inline = e.Elements
	case *slackgo.RichTextPreformatted:
		inline = e.Elements
	case *slackgo.RichTextList:
		var lines []string
		for _, item := range e.Elements {
			lines = append(lines, richTextLines(item)...)
		}
		return lines
	}
	var line strings.Builder
	for _, piece := range inline {
		switch p := piece.(type) {
		case *slackgo.RichTextSectionTextElement:
			line.WriteString(p.Text)
		case *slackgo.RichTextSectionLinkElement:
			line.WriteString(cmp.Or(p.Text, p.URL))
		}
	}
	if s := strings.TrimRight(line.String(), "\n"); s != "" {
		return []string{s}
	}
	return nil
}

// saysMore says whether a post's block words hold anything its text does not. Slack fills text from a section block and makes a
// rich_text block from plain text, so words often repeat it, in other markup: only the letters and digits are compared.
func saysMore(text, words string) bool {
	return words != "" && !strings.Contains(plainWords(text), plainWords(words))
}

// plainWords is the letters and digits of s, as a person reads it: a link by its label, and with no user, channel or special token.
func plainWords(s string) string {
	s = slackToken.ReplaceAllString(s, "")
	s = slackLink.ReplaceAllString(s, "$1")
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, html.UnescapeString(s))
}
