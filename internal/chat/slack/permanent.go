package slack

import (
	"errors"

	slackgo "github.com/slack-go/slack"
)

// permanentCode returns the Slack error code in err if it says that the same post will never
// succeed, and "" otherwise (including for an error that is not Slack's own). Those codes are a
// channel that is gone, archived or that Agent Kourier is not in, text that is too long, blocks Slack
// refuses, and an update that would take a message's text away (wouldDropText, measured); a rate limit, a server error or a timeout may pass on another try, and a rejected token is
// the restart policy's. It reads the code scrub keeps, through any wrapping.
//
// UNCONFIRMED(WP-04): that these are the codes Slack answers with, in particular both
// invalid_blocks and invalid_blocks_format for blocks it refuses, invalid_chunks for a chunk it refuses, and
// streaming_mode_mismatch and cannot_provide_both_markdown_text_and_chunks for a stream that mixes the two modes
// (documented, with chat.appendStream), and that no other code is
// permanent for a post (a channel that is not found can be a private one Agent Kourier was removed from).
func permanentCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "channel_not_found", "is_archived", "not_in_channel", "msg_too_long", "invalid_blocks", "invalid_blocks_format",
		"invalid_chunks", "streaming_mode_mismatch", "cannot_provide_both_markdown_text_and_chunks", "block_mismatch":
		return resp.Err
	}
	return ""
}

// reactionPermanentCode returns the Slack error code in err if it says that the same reaction will
// never be made, and "" otherwise: a message, a name or a channel that is not there or is archived,
// Agent Kourier not in the channel, no item or a bad ts given, a thread that is locked, a message that
// takes no reactions, or a limit on reactions or emoji reached (which a retry does not lift). It is
// not permanentCode, which is for posts; the two sets differ
// (a reaction has no text to be too long). A rate limit, a server error or a timeout may pass on
// another try. It reads the code scrub keeps, through any wrapping, so it is "" for an error whose
// text merely mentions a code.
//
// UNCONFIRMED(WP-04): that these are the codes Slack answers with, in particular for a message that
// was deleted and for a channel Agent Kourier was removed from, that thread_locked, not_reactable,
// too_many_reactions, too_many_emoji and bad_timestamp are Slack's codes and none passes on a
// retry, and that no other is permanent.
func reactionPermanentCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "message_not_found", "invalid_name", "no_item_specified", "not_in_channel", "channel_not_found",
		"is_archived", "thread_locked", "not_reactable", "too_many_reactions", "too_many_emoji", "bad_timestamp":
		return resp.Err
	}
	return ""
}

// fatalAuthCode returns the Slack error code in err if it says that the tokens will never work: a
// rejected, revoked, expired or missing token, or a deactivated account. Running again cannot cure it, so the
// process exits and says why, where any other error from Run is retried with a backoff. It is not
// permanentCode, which is about one post. It reads the code scrub keeps, through any wrapping, and
// is "" for an error that is not Slack's own, whatever its text says.
func fatalAuthCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "invalid_auth", "token_revoked", "token_expired", "account_inactive", "not_authed":
		return resp.Err
	}
	return ""
}

// groupsMisconfiguredCode returns the Slack error code in err if it says that usergroups.users.list will fail the same
// way on every try until an operator changes the configuration, and "" otherwise (including for an error that is not
// Slack's own). The codes are a group that does not exist (no_such_subteam), an app installed without usergroups:read
// (missing_scope), a workspace whose plan has no user groups (plan_upgrade_required), a token of a type the method does
// not take (not_allowed_token_type), a token without the permission (no_permission), and a method an Enterprise grid
// cannot call (enterprise_is_restricted). A rate limit, a server error, a timeout and a cancelled context may pass on
// another try. A rejected, revoked or missing token (invalid_auth, token_revoked, token_expired, account_inactive, not_authed) is
// fatalAuthCode's, the restart policy's, and is not reclassified here: the group read is as transient as any other
// call on a token that Run is about to find rejected. It reads the code scrub keeps, through any wrapping.
//
// From Slack's method page (docs.slack.dev/reference/methods/usergroups.users.list, read 2026-10-11), which lists each
// of these codes: no_such_subteam, missing_scope, plan_upgrade_required, not_allowed_token_type, no_permission and
// enterprise_is_restricted. The page does not say which of its codes are permanent; that each is, is this adapter's
// reading of what the code names.
//
// UNCONFIRMED(WP-04): that a live workspace answers with these codes for the causes named (in particular that a
// disabled group, asked for without include_disabled, is no_such_subteam and not an empty list, and that a free
// workspace gets plan_upgrade_required), and that no other code on the page is permanent for this call (invalid_arguments
// and access_denied are left transient: the first may come from a call Agent Kourier built wrong, and the second names no
// cause).
func groupsMisconfiguredCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "no_such_subteam", "missing_scope", "plan_upgrade_required", "not_allowed_token_type", "no_permission", "enterprise_is_restricted":
		return resp.Err
	}
	return ""
}

// channelAccessCode returns the Slack error code in err if it says that conversations.info will fail the same way on every
// try until an operator changes something, and "" otherwise (including for an error that is not Slack's own). The codes
// are a channel value Slack does not accept (channel_not_found: there is no such channel, or none the bot may see) and an
// app installed without the scope the method needs (missing_scope). A rate limit, a server error, a timeout and a
// cancelled context may pass on another try. A rejected, revoked or missing token (invalid_auth, token_revoked,
// token_expired, account_inactive, not_authed) is fatalAuthCode's, the restart policy's, and is not reclassified here, as for
// groupsMisconfiguredCode. The other codes of the method's page (access_denied, team_access_not_granted, no_permission,
// not_allowed_token_type, enterprise_is_restricted and the like) stay unclassified, and a caller retries them, because
// ChannelAccess has a sentinel for only those two causes and these name neither. That is not because they are transient:
// groupsMisconfiguredCode treats no_permission, not_allowed_token_type and enterprise_is_restricted as permanent for its
// method, and if conversations.info answers one of them on every try, a caller retries for as long as that lasts. It reads
// the code scrub keeps, through any wrapping.
// ChannelAccess wraps the sentinel of each of the two codes, and Platform.Classify(OpOther) reads the code back through that
// wrapping, for the Verdict's Code.
//
// From Slack's method page (docs.slack.dev/reference/methods/conversations.info/, read 2026-11-01), which lists both codes,
// describes channel_not_found as "Value passed for channel was invalid", and does not say which of its codes are permanent;
// that these two are, is this adapter's reading of what the codes name.
//
// UNCONFIRMED(BV-3), because the page is silent and no live workspace was asked: what Slack answers for a private channel
// the bot is not a member of. It may be channel_not_found, as for a channel that does not exist, or the channel with
// is_member false. The adapter handles both (ChannelAccess reads is_member when Slack answers with the channel), and the
// first reaches the caller as chat.ErrChannelNotFound, not as a bot that is not in the channel.
func channelAccessCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "channel_not_found", "missing_scope":
		return resp.Err
	}
	return ""
}
