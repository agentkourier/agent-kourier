package broker

import (
	"context"
	"log/slog"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// botOwners tells whose bot a bot ID is among the connections the broker runs.
type botOwners interface {
	// botOwner returns the connection ("namespace/name") whose bot has the ID botID, if one of the connections that run does.
	botOwner(botID string) (conn string, ok bool)
	// botUserOwner is botOwner for the user a bot posts as (appIdentity.User).
	botUserOwner(user string) (conn string, ok bool)
}

// botGuard is the Sink of a connection's platform: the Intake, except that a bot's mention by the bot of another connection that
// the broker runs starts nothing (spec D52). Two of the broker's apps in one channel would otherwise answer each other without end:
// a bot's mention starts a turn on any Binding that takes them (chat.botMentions), and each app's reply can mention the other. A
// Binding's trigger that names the other app's bot (a botMessage trigger with from.id) still reads its posts, because that is a
// message the Intake takes by the trigger and not as a mention (AcceptsBot, and AcceptsBotMention already says no for a bot a
// trigger names): "unless a trigger names the bot" is the Intake's rule, and this adds the other half, for a bot nobody named.
// The same goes for a post that reaches the Intake with no bot ID and the user of another connection's bot: the personMessage trigger and a thread's
// continuation read a post with no bot ID as a person's, and Slack's posts of another app carry a bot ID (docs/spikes/slack-mention-probe.md), so this
// is not expected, and a post of one of the broker's own apps taken as a person's would be the same loop. Everything else is the Intake's. A bot that is none of the broker's is the Intake's as before, so with one connection nothing changes.
type botGuard struct {
	in   *chat.Intake
	self string // the connection this sink is for
	bots botOwners
	log  *slog.Logger
}

var (
	_ chat.Sink             = botGuard{}
	_ chat.BotFilter        = botGuard{}
	_ chat.BotMentionFilter = botGuard{}
)

// other says whether botID is the bot of a connection that runs and that is not self.
func (g botGuard) other(botID string) bool {
	conn, ok := g.bots.botOwner(botID)
	return ok && conn != g.self
}

// otherUser says whether user is the user of another connection's bot.
func (g botGuard) otherUser(user string) bool {
	conn, ok := g.bots.botUserOwner(user)
	return ok && conn != g.self
}

// Message drops a bot's mention whose bot is another connection's, and a post with no bot ID whose user is, acknowledged, and gives the Intake
// everything else, a person's message and a bot's message that a trigger names. The adapter has asked AcceptsBotMention already; this is for an
// adapter that does not, and for the identity of a bot that became known after it asked.
func (g botGuard) Message(ctx context.Context, m chat.Inbound) error {
	switch {
	case m.Bot != nil && m.Mentioned && g.other(m.Bot.ID):
		g.log.DebugContext(ctx, "chat: a mention by the bot of another connection of this broker was dropped", "connection", g.self, "channel", m.At.Channel, "message", m.At.ID)
		return nil
	case m.Bot == nil && g.otherUser(m.User):
		g.log.DebugContext(ctx, "chat: a post by the user of another connection's bot, with no bot ID, was dropped, and not read as a person's", "connection", g.self, "channel", m.At.Channel, "message", m.At.ID)
		return nil
	}
	return g.in.Message(ctx, m)
}

// Interaction is the Intake's.
func (g botGuard) Interaction(ctx context.Context, i chat.Interaction) error {
	return g.in.Interaction(ctx, i)
}

// AcceptsBot is the Intake's: a botMessage trigger that names the bot is the only way a bot's message is taken, another connection's included.
func (g botGuard) AcceptsBot(channel, botID string) bool { return g.in.AcceptsBot(channel, botID) }

// AcceptsBotMention is the Intake's, and no for the bot of another connection that runs.
func (g botGuard) AcceptsBotMention(channel, botID string) bool {
	return !g.other(botID) && g.in.AcceptsBotMention(channel, botID)
}
