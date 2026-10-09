package slack

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// History reads the history of channel, every message since the given time (all of it, if since is zero), and
// returns it oldest first, each as the adapter would have seen it arrive: decoded by the code that decodes a live
// event, so a message that Run would drop is Ignored, Agent Kourier's own post is Own, and a bot's attachment is flattened
// into the fields a trigger reads. conn is the "namespace/name" of the ChatConnection, which the Refs name. The dry
// run replays it through a Binding's triggers (chat.Replay).
//
// It reads through the paging that Find uses, to the end, and fails when it cannot reach it. It reads the messages
// of the channel itself; the replies of a thread are not among them, but a reply also sent to the channel is.
// It changes nothing.
//
// UNCONFIRMED(WP-04): that a message in history has the shape of its live event (the fields decode reads:
// bot_id, subtype, user, text, attachments); the fake Slack's history is derived from the owner's channel.
func (c *Client) History(ctx context.Context, conn, channel string, since time.Time) ([]chat.HistoryMessage, error) {
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return nil, c.scrub("auth.test", err)
	}
	if auth.UserID == "" { // without it Agent Kourier cannot tell its own posts that arrive with no bot_id
		return nil, errors.New("slack auth.test: no bot user ID")
	}
	me := self{user: auth.UserID, bot: auth.BotID}
	var raw []slackgo.Message
	if err := c.walk(ctx, channel, "", since, func(msgs []slackgo.Message) bool {
		raw = append(raw, msgs...)
		return false
	}); err != nil {
		return nil, err
	}
	// Slack pages newest first, which nothing here relies on: the order is by ts.
	slices.SortStableFunc(raw, func(a, b slackgo.Message) int { return compareTS(a.Timestamp, b.Timestamp) })
	out := make([]chat.HistoryMessage, len(raw))
	for i, m := range raw {
		out[i] = historyMessage(me, conn, channel, m)
	}
	return out, nil
}

// historyMessage decodes a message of the history as decode does a live event, by making the event it would have
// been. A history message has no channel_type; a channel the dry run reads is not a direct message.
func historyMessage(me self, conn, channel string, m slackgo.Message) chat.HistoryMessage {
	msg, v := decode(me, &slackevents.MessageEvent{
		Type: "message", User: m.User, Text: m.Text, ThreadTimeStamp: m.ThreadTimestamp, TimeStamp: m.Timestamp,
		Channel: channel, ChannelType: slackevents.ChannelTypeChannel, SubType: m.SubType, BotID: m.BotID, Username: m.Username,
		Message: &m.Msg,
	})
	if v == dropped { // only where it was is kept, so that the dry run can list it
		return chat.HistoryMessage{Message: inboundOf(conn, Message{Channel: channel, TS: m.Timestamp, ThreadTS: m.ThreadTimestamp}), Ignored: true}
	}
	return chat.HistoryMessage{Message: inboundOf(conn, msg), Own: v == ownPost}
}

// compareTS orders two message timestamps, "seconds.microseconds", without parsing them: by the length of the
// seconds, then the digits, then the fraction. Slack's ts are fixed-width, so the length rarely matters.
func compareTS(a, b string) int {
	as, af, _ := strings.Cut(a, ".")
	bs, bf, _ := strings.Cut(b, ".")
	return cmp.Or(cmp.Compare(len(as), len(bs)), strings.Compare(as, bs), strings.Compare(af+"000000"[min(len(af), 6):], bf+"000000"[min(len(bf), 6):]))
}
