package slack

import (
	"errors"

	slackgo "github.com/slack-go/slack"
)

// PermanentCode returns the Slack error code in err if it says that the same post will never
// succeed, and "" otherwise (including for an error that is not Slack's own). Those codes are a
// channel that is gone, archived or that courier is not in, text that is too long, and blocks Slack
// refuses; a rate limit, a server error or a timeout may pass on another try, and a rejected token is
// the restart policy's. It reads the code scrub keeps, through any wrapping.
//
// UNCONFIRMED(WP-04): that these are the codes Slack answers with, in particular both
// invalid_blocks and invalid_blocks_format for blocks it refuses, and that no other code is
// permanent for a post (a channel that is not found can be a private one courier was removed from).
func PermanentCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "channel_not_found", "is_archived", "not_in_channel", "msg_too_long", "invalid_blocks", "invalid_blocks_format":
		return resp.Err
	}
	return ""
}

// ReactionPermanentCode returns the Slack error code in err if it says that the same reaction will
// never be made, and "" otherwise: a message, a name or a channel that is not there or is archived,
// courier not in the channel, no item or a bad ts given, a thread that is locked, a message that
// takes no reactions, or a limit on reactions or emoji reached (which a retry does not lift). It is
// not PermanentCode, which is for posts; the two sets differ
// (a reaction has no text to be too long). A rate limit, a server error or a timeout may pass on
// another try. It reads the code scrub keeps, through any wrapping, so it is "" for an error whose
// text merely mentions a code.
//
// UNCONFIRMED(WP-04): that these are the codes Slack answers with, in particular for a message that
// was deleted and for a channel courier was removed from, that thread_locked, not_reactable,
// too_many_reactions, too_many_emoji and bad_timestamp are Slack's codes and none passes on a
// retry, and that no other is permanent.
func ReactionPermanentCode(err error) string {
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

// FatalAuthCode returns the Slack error code in err if it says that the tokens will never work: a
// rejected, revoked or missing token, or a deactivated account. Running again cannot cure it, so the
// process exits and says why, where any other error from Run is retried with a backoff. It is not
// PermanentCode, which is about one post. It reads the code scrub keeps, through any wrapping, and
// is "" for an error that is not Slack's own, whatever its text says.
func FatalAuthCode(err error) string {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return ""
	}
	switch resp.Err {
	case "invalid_auth", "token_revoked", "account_inactive", "not_authed":
		return resp.Err
	}
	return ""
}
