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

// StreamMode is how the text of a stream travels. Slack documents that a stream keeps the mode it began in
// (chat.stopStream: "The streaming mode must be the same as the streaming mode used to start the stream. If you
// started the stream with markdown_text, you must append with markdown_text"), so the mode is chosen when the stream
// starts, and every later call of the stream uses it.
type StreamMode int

const (
	// ChunksMode sends the text as markdown_text chunks. It is the mode of a stream that can show steps, since a
	// task_update chunk cannot ride a stream in the other mode, and the one a stream starts in.
	//
	// UNCONFIRMED(WP-04): that a stream whose text is all chunks reads and ends as one whose text was markdown_text
	// (confirmed in dogfood, 2026-10-03, for markdown_text: a stop's blocks are appended under the streamed text).
	ChunksMode StreamMode = iota
	// MarkdownMode sends the text as the markdown_text parameter. A stream in it shows no steps. It is the fallback
	// of a Slack that refuses the chunks mode (Platform.Stream).
	MarkdownMode
)

// text is the option that sends markdown as the text of a streaming request in the mode.
func (m StreamMode) text(markdown string) slackgo.MsgOption {
	if m == MarkdownMode {
		return slackgo.MsgOptionMarkdownText(markdown)
	}
	return slackgo.MsgOptionChunks(slackgo.NewMarkdownTextChunk(markdown))
}

// StartStream starts a streaming message in the thread threadTS of channel, with markdown as
// its first text, in mode, shown to recipientUser, and returns its ts. The message exists from this call
// on and cannot be found again by metadata (spec 8.6 step 6), so record the ts at once. The
// error holds no token.
//
// UNCONFIRMED(WP-04): that a channel needs both recipient_user_id and recipient_team_id, and
// that a stream may start with no content at all (slack-go's example does), and then be in the
// mode of its first append.
func (c *Client) StartStream(ctx context.Context, channel, threadTS, recipientUser, markdown string, mode StreamMode) (telemetryResult0 string, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.StartStream")
	defer func() { finishChat(ctx, op, "StartStream", telemetryErr) }()
	team, err := c.TeamID(ctx)
	if err != nil {
		return "", err
	}
	opts := append(inertOptions(), slackgo.MsgOptionTS(threadTS), slackgo.MsgOptionRecipientUserID(recipientUser), slackgo.MsgOptionRecipientTeamID(team))
	if markdown != "" {
		opts = append(opts, mode.text(markdown))
	}
	_, ts, err := c.api.StartStreamContext(ctx, channel, opts...)
	if err != nil {
		return "", c.scrub("chat.startStream", err)
	}
	return ts, nil
}

// AppendStream adds markdown, in the mode of the stream, to the end of the streaming message ts. The error holds no
// token.
func (c *Client) AppendStream(ctx context.Context, channel, ts, markdown string, mode StreamMode) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.AppendStream")
	defer func() { finishChat(ctx, op, "AppendStream", telemetryErr) }()
	if _, _, err := c.api.AppendStreamContext(ctx, channel, ts, append(inertOptions(), mode.text(markdown))...); err != nil {
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
// Slack merges the chunks of one ID into one card (observed 2026-10-18 in two workspaces, read back through the API and
// in the web client): a task_update with an ID the stream has seen updates that card and does not add another; its
// title and status replace the card's; its details and output are appended to the card's, so a chunk that repeats
// them shows them twice; and a chunk that leaves them out, or sends them empty, keeps what the card had. AppendStep
// sends u as it is, and the stream (Step) sends details and output once.
func (c *Client) AppendStep(ctx context.Context, channel, ts string, u TaskUpdate) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.AppendStep")
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
// message found by that tag is one Agent Kourier posted (spec 8.6). Like Post, it refuses an empty
// outboxID. The error holds no token.
//
// It takes markdown and blocks, not a Content: a stream's text is appended as markdown_text (in the mode of the
// stream), and has no separate plain text.
//
// UNCONFIRMED(WP-04): that chat.stopStream accepts blocks and metadata, which chat.startStream
// and chat.appendStream do not.
func (c *Client) StopStream(ctx context.Context, channel, ts, markdown string, blocks json.RawMessage, outboxID string, mode StreamMode) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.StopStream")
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
		opts = append(opts, mode.text(markdown))
	}
	if _, _, err := c.api.StopStreamContext(ctx, channel, ts, opts...); err != nil {
		return c.scrub("chat.stopStream", err)
	}
	return nil
}
