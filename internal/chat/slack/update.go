package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackgo "github.com/slack-go/slack"
)

// Update replaces the text of the message ts in channel, and its blocks if blocks is not empty.
// Send both together: a request that omits blocks may leave them, or clear them.
// A message that is still streaming cannot be updated; stop the stream first. The error holds
// no token.
//
// UNCONFIRMED(WP-04): that chat.update keeps the message's metadata (the outbox looks a message
// up only before its ts is recorded, so courier does not need it to), what it does with omitted
// blocks, and the error code for a message still streaming (the fake says cant_update_message).
func (c *Client) Update(ctx context.Context, channel, ts, text string, blocks json.RawMessage) error {
	opts, err := blockOptions(blocks)
	if err != nil {
		return fmt.Errorf("slack update: %w", err)
	}
	if _, _, _, err := c.api.UpdateMessageContext(ctx, channel, ts, append(opts, slackgo.MsgOptionText(text, false))...); err != nil {
		return c.scrub("chat.update", err)
	}
	return nil
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
