package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackgo "github.com/slack-go/slack"
)

// Update replaces the text of the message ts in channel with content.Text, and its blocks with
// content.Blocks: `[]` clears them, and none leaves the field out of the request. Every Update of
// a message that has blocks must send them again, because what Slack does with omitted blocks is
// UNCONFIRMED(WP-04), and the message may lose them. A message that is still streaming cannot be
// updated; stop the stream first. The error holds no token.
//
// UNCONFIRMED(WP-04): that chat.update keeps the message's metadata (the outbox looks a message
// up only before its ts is recorded, so courier does not need it to), and the error code for a
// message still streaming (the fake says cant_update_message).
func (c *Client) Update(ctx context.Context, channel, ts string, content Content) error {
	opts, err := blockOptions(content.Blocks)
	if err != nil {
		return fmt.Errorf("slack update: %w", err)
	}
	opts = append(opts, inertOptions()...)
	if _, _, _, err := c.api.UpdateMessageContext(ctx, channel, ts, append(opts, slackgo.MsgOptionText(content.Text, false))...); err != nil {
		return c.scrub("chat.update", err)
	}
	return nil
}

// inertOptions returns the options every post, update and stream of courier carries, so that
// agent-written text stays text (spec 8.4): no parse, which would turn @here and @channel into
// broadcasts, no link_names, and no unfurling, which makes Slack's servers fetch a URL the agent
// wrote with no one clicking it. A method that sends a message must include them.
//
// UNCONFIRMED(WP-04): that chat.startStream, chat.appendStream and chat.stopStream accept the
// unfurl options (the fake ignores any parameter it does not read).
func inertOptions() []slackgo.MsgOption {
	return []slackgo.MsgOption{slackgo.MsgOptionParse(false), slackgo.MsgOptionDisableLinkUnfurl(), slackgo.MsgOptionDisableMediaUnfurl()}
}

// blockOptions returns the option that sets raw, Block Kit blocks as JSON, or none for no blocks.
func blockOptions(raw json.RawMessage) ([]slackgo.MsgOption, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("blocks: %w", err)
	}
	return []slackgo.MsgOption{slackgo.MsgOptionBlocks(blocks.BlockSet...)}, nil
}
