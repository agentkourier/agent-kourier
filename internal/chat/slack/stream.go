package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	slackgo "github.com/slack-go/slack"
)

// TeamID returns the workspace's team ID, which chat.startStream needs in a channel. It asks
// auth.test the first time and keeps the answer.
func (c *Client) TeamID(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.team
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return "", c.scrub("auth.test", err)
	}
	if auth.TeamID == "" {
		return "", errors.New("slack auth.test: no team ID")
	}
	c.mu.Lock()
	c.team = auth.TeamID
	c.mu.Unlock()
	return auth.TeamID, nil
}

// StartStream starts a streaming message in the thread threadTS of channel, with markdown as
// its first text, shown to recipientUser, and returns its ts. The message exists from this call
// on and cannot be found again by metadata (spec 8.6 step 6), so record the ts at once. The
// error holds no token.
//
// UNCONFIRMED(WP-04): that a channel needs both recipient_user_id and recipient_team_id, and
// that the first text may be empty.
func (c *Client) StartStream(ctx context.Context, channel, threadTS, recipientUser, markdown string) (string, error) {
	team, err := c.TeamID(ctx)
	if err != nil {
		return "", err
	}
	opts := []slackgo.MsgOption{slackgo.MsgOptionTS(threadTS), slackgo.MsgOptionRecipientUserID(recipientUser), slackgo.MsgOptionRecipientTeamID(team)}
	if markdown != "" {
		opts = append(opts, slackgo.MsgOptionMarkdownText(markdown))
	}
	_, ts, err := c.api.StartStreamContext(ctx, channel, opts...)
	if err != nil {
		return "", c.scrub("chat.startStream", err)
	}
	return ts, nil
}

// AppendStream adds markdown to the end of the streaming message ts. The error holds no token.
func (c *Client) AppendStream(ctx context.Context, channel, ts, markdown string) error {
	if _, _, err := c.api.AppendStreamContext(ctx, channel, ts, slackgo.MsgOptionMarkdownText(markdown)); err != nil {
		return c.scrub("chat.appendStream", err)
	}
	return nil
}

// StopStream adds the last markdown, if any, and ends the streaming message ts, setting its
// blocks if not empty (`[]` clears them), and tagging it with metadata that names outboxID: a
// message found by that tag is one courier posted (spec 8.6). Like Post, it refuses an empty
// outboxID. The error holds no token.
//
// It takes markdown and blocks, not a Content: a stream's text is appended as markdown_text, and
// has no separate plain text.
//
// UNCONFIRMED(WP-04): that chat.stopStream accepts blocks and metadata, which chat.startStream
// and chat.appendStream do not.
func (c *Client) StopStream(ctx context.Context, channel, ts, markdown string, blocks json.RawMessage, outboxID string) error {
	tag, err := outboxTag(outboxID)
	if err != nil {
		return fmt.Errorf("slack stop stream: %w", err)
	}
	opts, err := blockOptions(blocks)
	if err != nil {
		return fmt.Errorf("slack stop stream: %w", err)
	}
	opts = append(opts, tag)
	if markdown != "" {
		opts = append(opts, slackgo.MsgOptionMarkdownText(markdown))
	}
	if _, _, err := c.api.StopStreamContext(ctx, channel, ts, opts...); err != nil {
		return c.scrub("chat.stopStream", err)
	}
	return nil
}
