package slack

import (
	"context"
	"time"

	slackgo "github.com/slack-go/slack"
)

// membersTimeout bounds IsChannelMember as a whole, every page of it: a press waits on the answer,
// and the caller's context may have no deadline.
const membersTimeout = 2 * time.Second

// PostEphemeral shows content to user alone in channel (chat.postEphemeral), with the inert options
// every message of courier carries. Nothing else sees it, and Slack gives it no event and no way to
// update it. A user outside the channel is refused by Slack; the error holds no token.
//
// UNCONFIRMED(WP-04): that chat.postEphemeral accepts the unfurl and parse options, and the error
// code for a recipient outside the channel (the fake says user_not_in_channel).
func (c *Client) PostEphemeral(ctx context.Context, channel, user string, content Content) error {
	opts, err := blockOptions(content.Blocks)
	if err != nil {
		return err
	}
	opts = append(opts, slackgo.MsgOptionText(content.Text, false))
	opts = append(opts, inertOptions()...)
	if _, err := c.api.PostEphemeralContext(ctx, channel, user, opts...); err != nil {
		return c.scrub("chat.postEphemeral", err)
	}
	return nil
}

// IsChannelMember reports whether user is a member of channel, reading conversations.members to its
// last page because no order is assumed, and stopping early only at a hit. It fails closed: any
// error, a page that fails, the 2 second bound included, is false and the error, even if the user
// was on a page not yet read. The error holds no token.
//
// UNCONFIRMED(WP-04): that conversations.members lists a private channel's members to a bot that is
// in it, and that its pages follow next_cursor to an empty one.
func (c *Client) IsChannelMember(ctx context.Context, channel, user string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, membersTimeout)
	defer cancel()
	cursor := ""
	for {
		members, next, err := c.api.GetUsersInConversationContext(ctx, &slackgo.GetUsersInConversationParameters{ChannelID: channel, Cursor: cursor, Limit: c.pageSize})
		if err != nil {
			return false, c.scrub("conversations.members", err)
		}
		for _, m := range members {
			if m == user {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		cursor = next
	}
}

// IsBot reports whether user is a bot (users.info is_bot), uncached: ResolveUser's cache does not
// hold the flag. It fails closed too: on an error it is true, with the error, so a caller that does
// not look at the error still refuses. The error holds no token.
//
// UNCONFIRMED(WP-04): that is_bot is set for every kind of bot user, workflow and app bots included.
func (c *Client) IsBot(ctx context.Context, user string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u, err := c.api.GetUserInfoContext(ctx, user)
	if err != nil {
		return true, c.scrub("users.info", err)
	}
	return u.IsBot, nil
}
