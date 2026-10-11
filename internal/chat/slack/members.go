package slack

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	// authzTimeout is the default bound of IsChannelMember (as a whole, every page of it), of IsInAnyUserGroup (as a whole, every group of it) and of IsBot:
	// a press waits on the answers, and the caller's context may have no deadline. Config.AuthzTimeout
	// changes it, for a caller that is not on Slack's 3 second acknowledgement path.
	authzTimeout = 2 * time.Second
	// membersPage is conversations.members' largest page, so a big channel takes few calls within the bound.
	membersPage = 1000
)

// PostEphemeral shows content to user alone in channel (chat.postEphemeral), in the thread threadTS
// if set (a prompt lives in a thread, and a notice outside it shows in the channel's main view where
// the person may never see it), with the inert options every message of Agent Kourier carries. Nothing else
// sees it, and Slack gives it no event and no way to update it. A user outside the channel is refused
// by Slack. Bad blocks are store.ErrInvalid. The error holds no token. The app needs the chat:write scope.
//
// UNCONFIRMED(WP-04): that chat.postEphemeral accepts the unfurl and parse options and thread_ts, and
// the error code for a recipient outside the channel (the fake says user_not_in_channel).
func (c *Client) PostEphemeral(ctx context.Context, channel, threadTS, user string, content Content) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.PostEphemeral")
	defer func() { finishChat(ctx, op, "PostEphemeral", telemetryErr) }()
	opts, err := blockOptions(content.Blocks)
	if err != nil {
		return fmt.Errorf("slack ephemeral: %w: %w", store.ErrInvalid, err)
	}
	opts = append(opts, slackgo.MsgOptionText(content.Text, false))
	opts = append(opts, inertOptions()...)
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	if _, err := c.api.PostEphemeralContext(ctx, channel, user, opts...); err != nil {
		return c.scrub("chat.postEphemeral", err)
	}
	return nil
}

// IsChannelMember reports whether user is a member of channel, reading conversations.members to its
// last page because no order is assumed, and stopping early only at a hit. It fails closed: any
// error, a page that fails, the bound included (2 seconds, or Config.AuthzTimeout), is false and the
// error, even if the user was on a page not yet read. The error holds no token. The app needs the
// channels:read scope, and groups:read for private channels.
//
// UNCONFIRMED(WP-04): that the member list holds bots, deactivated users and Slack Connect external
// members as Agent Kourier assumes (a bot or a deactivated user is told apart by IsBot, not here); that a
// private channel lists its members only to a bot that is in it; and that its pages follow
// next_cursor to an empty one.
func (c *Client) IsChannelMember(ctx context.Context, channel, user string) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.IsChannelMember")
	defer func() { finishChat(ctx, op, "IsChannelMember", telemetryErr) }()
	ctx, cancel := context.WithTimeout(ctx, c.authz)
	defer cancel()
	cursor := ""
	for {
		members, next, err := c.api.GetUsersInConversationContext(ctx, &slackgo.GetUsersInConversationParameters{ChannelID: channel, Cursor: cursor, Limit: c.membersPage})
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

// IsInAnyUserGroup reports whether user is a member of at least one of the user groups, whose IDs are Slack's
// (usergroups.users.list, one call per group, in order, stopping at the first that lists user). Nothing is cached: a
// person taken out of a group is refused at the next call. It fails closed: an error from any group read, the bound
// included (2 seconds for all the groups together, or Config.AuthzTimeout), is false and the error, even if a later
// group lists user. No groups or no user is false with no call and no error: the caller decides what no groups
// means. Disabled groups are not asked for (include_disabled is not sent). The error holds no token. The app needs
// the usergroups:read scope.
//
// From Slack's method page (docs.slack.dev/reference/methods/usergroups.users.list, read 2026-10-11): the scope is
// usergroups:read for a bot token; the call takes usergroup and the optional include_disabled and team_id, and has
// no cursor; an unknown ID is no_such_subteam ("No usergroup found with the given ID"); and the page lists the error
// plan_upgrade_required ("only available on Standard and above plans"). Slack's help center article "Create a user
// group" (https://slack.com/help/articles/212906697-Create-a-user-group) says user groups are "available on paid
// plans".
//
// UNCONFIRMED(WP-04), because the page is silent or no live workspace was asked: what Slack answers for a disabled
// group when include_disabled is not sent (an error, or a list), and so whether a disabled group's members would
// count (the fake has no disabled groups); that a bot token with only usergroups:read is enough in a real workspace; that the
// error for a missing usergroup is invalid_arguments (the fake says it); and whether a free workspace gets
// plan_upgrade_required or another code.
func (c *Client) IsInAnyUserGroup(ctx context.Context, groups []string, user string) (telemetryResult0 bool, telemetryErr error) {
	if len(groups) == 0 || user == "" {
		return false, nil
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.IsInAnyUserGroup")
	defer func() { finishChat(ctx, op, "IsInAnyUserGroup", telemetryErr) }()
	ctx, cancel := context.WithTimeout(ctx, c.authz)
	defer cancel()
	for _, group := range groups {
		members, err := c.api.GetUserGroupMembersContext(ctx, group)
		if err != nil {
			return false, c.scrub("usergroups.users.list", err)
		}
		if slices.Contains(members, user) {
			return true, nil
		}
	}
	return false, nil
}

// IsBot reports whether user is a bot (users.info is_bot), uncached: ResolveUser's cache does not
// hold the flag. It fails closed too: on an error it is true, with the error, so a caller that does
// not look at the error still refuses. It is bounded like IsChannelMember (2 seconds, or
// Config.AuthzTimeout). The error holds no token. The app needs the users:read scope.
//
// UNCONFIRMED(WP-04): that is_bot is set for every kind of bot user, workflow and app bots included,
// and what users.info says of a deactivated user.
func (c *Client) IsBot(ctx context.Context, user string) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.IsBot")
	defer func() { finishChat(ctx, op, "IsBot", telemetryErr) }()
	ctx, cancel := context.WithTimeout(ctx, c.authz)
	defer cancel()
	u, err := c.api.GetUserInfoContext(ctx, user)
	if err != nil {
		return true, c.scrub("users.info", err)
	}
	return u.IsBot, nil
}
