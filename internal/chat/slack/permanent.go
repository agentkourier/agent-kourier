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
