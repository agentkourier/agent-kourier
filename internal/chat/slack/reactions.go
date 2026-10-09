package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// React adds the reaction name (the bare name, as in "hourglass", with no colons) to the message ts
// in channel (reactions.add), which marks a reply that waits for the active task (spec 8.2). A reaction
// that is there already, from this app, is success: the call is for a state, so a retry after a lost
// answer is not an error. An empty channel, ts or name is store.ErrInvalid, and no call is made. The
// error holds no token and keeps the Slack code for errors.As. The app needs the reactions:write
// scope.
//
// UNCONFIRMED(WP-04): that already_reacted is the code for a reaction this app added before (the
// fake says so), and which codes a message that is gone gives (the fake says message_not_found).
func (c *Client) React(ctx context.Context, channel, ts, name string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.React")
	defer func() { finishChat(ctx, op, "React", telemetryErr) }()
	if channel == "" || ts == "" || name == "" {
		return fmt.Errorf("slack react: the channel, ts and name are required: %w", store.ErrInvalid)
	}
	err := c.api.AddReactionContext(ctx, name, slackgo.ItemRef{Channel: channel, Timestamp: ts})
	var resp slackgo.SlackErrorResponse
	if errors.As(err, &resp) && resp.Err == "already_reacted" {
		return nil
	}
	if err != nil {
		return c.scrub("reactions.add", err)
	}
	return nil
}

// Unreact takes the reaction name (the bare name, with no colons) that this app put on the message ts in
// channel off it (reactions.remove), which ends the marker of a reply that waited for the active task.
// A reaction that is not there is success, as React treats one that is: the call is for a state, so a retry
// after a lost answer is not an error, and a marker that was never made needs no removing. An empty
// channel, ts or name is store.ErrInvalid, and no call is made. The error holds no token and keeps the
// Slack code for errors.As. The app needs the reactions:write scope.
//
// UNCONFIRMED(WP-04): that no_reaction is the code for a reaction that is not on the message (the fake
// says so), and which codes a message that is gone gives (the fake says message_not_found).
func (c *Client) Unreact(ctx context.Context, channel, ts, name string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Unreact")
	defer func() { finishChat(ctx, op, "Unreact", telemetryErr) }()
	if channel == "" || ts == "" || name == "" {
		return fmt.Errorf("slack unreact: the channel, ts and name are required: %w", store.ErrInvalid)
	}
	err := c.api.RemoveReactionContext(ctx, name, slackgo.ItemRef{Channel: channel, Timestamp: ts})
	var resp slackgo.SlackErrorResponse
	if errors.As(err, &resp) && resp.Err == "no_reaction" {
		return nil
	}
	if err != nil {
		return c.scrub("reactions.remove", err)
	}
	return nil
}
