package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

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

// textChunks is the option that sends markdown as the text of a streaming request. Every stream is in chunks mode,
// and never in markdown_text mode, because Slack documents that a stream keeps the mode it began in
// (chat.stopStream: "The streaming mode must be the same as the streaming mode used to start the stream. If you
// started the stream with markdown_text, you must append with markdown_text") and a stream cannot take a task_update
// chunk, which is how a step is shown, in the other mode. The text is a markdown_text chunk, which every streaming
// method documents.
//
// UNCONFIRMED(WP-04): that a stream whose text is all chunks reads and ends as one whose text was markdown_text
// (confirmed in dogfood, 2026-10-03, for markdown_text: a stop's blocks are appended under the streamed text).
func textChunks(markdown string) slackgo.MsgOption {
	return slackgo.MsgOptionChunks(slackgo.NewMarkdownTextChunk(markdown))
}

// StartStream starts a streaming message in the thread threadTS of channel, with markdown as
// its first text, shown to recipientUser, and returns its ts. The message exists from this call
// on and cannot be found again by metadata (spec 8.6 step 6), so record the ts at once. The
// error holds no token.
//
// UNCONFIRMED(WP-04): that a channel needs both recipient_user_id and recipient_team_id, and
// that a stream may start with no content at all (slack-go's example does), and then be in the
// mode of its first append.
func (c *Client) StartStream(ctx context.Context, channel, threadTS, recipientUser, markdown string) (telemetryResult0 string, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.chat.StartStream")
	defer func() { finishChat(ctx, op, "StartStream", telemetryErr) }()
	team, err := c.TeamID(ctx)
	if err != nil {
		return "", err
	}
	opts := append(inertOptions(), slackgo.MsgOptionTS(threadTS), slackgo.MsgOptionRecipientUserID(recipientUser), slackgo.MsgOptionRecipientTeamID(team))
	if markdown != "" {
		opts = append(opts, textChunks(markdown))
	}
	_, ts, err := c.api.StartStreamContext(ctx, channel, opts...)
	if err != nil {
		return "", c.scrub("chat.startStream", err)
	}
	return ts, nil
}

// AppendStream adds markdown to the end of the streaming message ts. The error holds no token.
func (c *Client) AppendStream(ctx context.Context, channel, ts, markdown string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.chat.AppendStream")
	defer func() { finishChat(ctx, op, "AppendStream", telemetryErr) }()
	if _, _, err := c.api.AppendStreamContext(ctx, channel, ts, append(inertOptions(), textChunks(markdown))...); err != nil {
		return c.scrub("chat.appendStream", err)
	}
	return nil
}

// TaskUpdate is one task_update chunk of a streaming message: a step of the agent's work, which Slack shows as a card
// (https://docs.slack.dev/reference/methods/chat.appendStream/). Every field is text that is ready to send: cleaned,
// escaped and cut. ID names the task in the stream, and a chunk with an ID the stream has seen is the same task in its
// new state. Status is in_progress, complete or error.
type TaskUpdate struct {
	ID, Title, Status, Details, Output string
}

// AppendStep adds a task_update chunk to the end of the streaming message ts, which is a step begun, or a step that
// moves to its outcome. The error holds no token.
//
// UNCONFIRMED(WP-04): that a task_update with an ID the stream has seen updates that card and does not add another,
// that it replaces and does not merge, and how Slack renders details and output (the documentation lists them, with
// no more than "additional descriptive information" and "results or completion data").
func (c *Client) AppendStep(ctx context.Context, channel, ts string, u TaskUpdate) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.chat.AppendStep")
	defer func() { finishChat(ctx, op, "AppendStep", telemetryErr) }()
	chunk := slackgo.NewTaskUpdateChunk(u.ID, u.Title)
	chunk.Status, chunk.Details, chunk.Output = slackgo.TaskCardStatus(u.Status), u.Details, u.Output
	if _, _, err := c.api.AppendStreamContext(ctx, channel, ts, append(inertOptions(), slackgo.MsgOptionChunks(chunk))...); err != nil {
		return c.scrub("chat.appendStream", err)
	}
	return nil
}

// StopStream adds the last markdown, if any, and ends the streaming message ts, appending blocks,
// if any, under the text that was streamed (Slack keeps that text and does not replace it:
// confirmed in dogfood, 2026-10-03), and tagging it with metadata that names outboxID: a
// message found by that tag is one courier posted (spec 8.6). Like Post, it refuses an empty
// outboxID. The error holds no token.
//
// It takes markdown and blocks, not a Content: a stream's text is appended as markdown_text, and
// has no separate plain text.
//
// UNCONFIRMED(WP-04): that chat.stopStream accepts blocks and metadata, which chat.startStream
// and chat.appendStream do not.
func (c *Client) StopStream(ctx context.Context, channel, ts, markdown string, blocks json.RawMessage, outboxID string) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.chat.StopStream")
	defer func() { finishChat(ctx, op, "StopStream", telemetryErr) }()
	tag, err := outboxTag(outboxID)
	if err != nil {
		return fmt.Errorf("slack stop stream: %w", err)
	}
	opts, err := blockOptions(blocks)
	if err != nil {
		return fmt.Errorf("slack stop stream: %w", err)
	}
	opts = append(opts, tag)
	opts = append(opts, inertOptions()...)
	if markdown != "" {
		opts = append(opts, textChunks(markdown))
	}
	if _, _, err := c.api.StopStreamContext(ctx, channel, ts, opts...); err != nil {
		return c.scrub("chat.stopStream", err)
	}
	return nil
}
