package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/agentkourier/agent-kourier/internal/chat"
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
		return "", c.scrub("auth.test call", err)
	}
	if auth.TeamID == "" {
		return "", errors.New("slack auth.test call: no team ID")
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
	defer func() { c.finishChat(ctx, op, "StartStream", telemetryErr) }()
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
	defer func() { c.finishChat(ctx, op, "AppendStream", telemetryErr) }()
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
	defer func() { c.finishChat(ctx, op, "AppendStep", telemetryErr) }()
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
	defer func() { c.finishChat(ctx, op, "StopStream", telemetryErr) }()
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

// Stream starts a streaming message in t, shown to the user to, with the markdown first. The stream starts in the
// chunks mode, which is the one that can show steps. That mode is a guess about Slack that no one has confirmed
// (UNCONFIRMED(WP-04)), and a Slack that refuses it, with one of the codes of refusesChunks, must not cost Agent Kourier its
// replies: the start is made once more in the markdown_text mode, and if that works the platform stays in it for
// the process, so that the next stream starts there with no failed call first, and is logged once. A stream in that
// mode shows no steps. A start with no text carries no mode and cannot be refused for it.
func (p *Platform) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	mode := p.streamMode()
	var esc mdText // the stream is one document, read from its first text on
	text := esc.text(md, maxMarkdown, false)
	ts, err := p.c.StartStream(ctx, t.Channel, t.Thread, to, text, mode)
	if err != nil && mode == ChunksMode && text != "" && refusesChunks(err) {
		first := err
		mode = MarkdownMode
		if ts, err = p.c.StartStream(ctx, t.Channel, t.Thread, to, text, mode); err != nil {
			return nil, err // the mode was not the trouble: the first answer is not the one to keep, nor is the latch set
		}
		p.fallBack(ctx, first)
	}
	if err != nil {
		return nil, err
	}
	s := &stream{p: p, ref: p.ref(t.Channel, t.Thread, ts), esc: esc}
	s.markdown.Store(mode == MarkdownMode)
	return s, nil
}

// streamMode is the mode a stream of this platform starts in.
func (p *Platform) streamMode() StreamMode {
	if p.markdown.Load() {
		return MarkdownMode
	}
	return ChunksMode
}

// fallBack latches the markdown_text mode, and logs it once at WARN with Slack's answer.
func (p *Platform) fallBack(ctx context.Context, refused error) {
	p.markdown.Store(true)
	p.warned.Do(func() {
		p.log.WarnContext(ctx, "slack: the chunks mode of a stream was refused; streams are in markdown_text mode from now on and show no steps",
			"conn", p.conn, "err", refused)
	})
}

// refusesChunks says whether err is the way Slack refuses a request for its mode: a chunk it does not take, a stream
// whose mode is another, both parameters at once, or arguments it does not take at all. The code is read from Slack's
// answer, never matched in text.
// UNCONFIRMED(WP-04): the codes, which are documented for chat.appendStream and chat.stopStream (the first three), and
// the last, which a stream started with chunks it cannot read may be answered with.
func refusesChunks(err error) bool {
	var resp slackgo.SlackErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	switch resp.Err {
	case "invalid_chunks", "streaming_mode_mismatch", "cannot_provide_both_markdown_text_and_chunks", "invalid_arguments":
		return true
	}
	return false
}

type stream struct {
	p        *Platform
	ref      chat.Ref
	markdown atomic.Bool // the stream is in MarkdownMode, which a refusal of the chunks mode changes it to

	// mu orders the texts and the steps of the stream: esc is the reading of the one document that Slack makes of the
	// texts (mdText), so a text is read and sent before the next is read, and shown is what each step's card holds.
	mu    sync.Mutex
	esc   mdText
	shown map[string]card
}

// card is what a step's card in Slack holds of the fields that Slack appends to: whether its details and its output
// were sent.
type card struct{ details, output bool }

func (s *stream) Ref() chat.Ref { return s.ref }

// streamMode is the mode the stream is in.
func (s *stream) streamMode() StreamMode {
	if s.markdown.Load() {
		return MarkdownMode
	}
	return ChunksMode
}

// text makes a call that carries text, in the mode of the stream. A stream that was started with no text has no mode
// yet, and the first text may be what Slack refuses in chunks: it is sent once more in markdown_text, and if that works
// the stream, and the platform, are in that mode from then on. One that is refused in both is no mode's trouble, and
// the first answer is kept.
//
// It also says whether the text may have reached Slack although the caller is told of an error: a call of the two that
// was lost on the way, with no answer from Slack, may have been applied (settle).
func (s *stream) text(ctx context.Context, md string, call func(StreamMode) error) (unsure bool, err error) {
	mode := s.streamMode()
	err = call(mode)
	unsure = err != nil && !refused(err)
	if err != nil && mode == ChunksMode && md != "" && refusesChunks(err) {
		retry := call(MarkdownMode)
		if retry == nil {
			s.markdown.Store(true)
			s.p.fallBack(ctx, err)
			return false, nil
		}
		unsure = unsure || !refused(retry)
	}
	return unsure, err
}

// refused says whether err is an answer of Slack that it did not apply the call: an error response, or a limit to wait
// out. Any other error (a connection that broke, a timeout) leaves it unknown.
func refused(err error) bool {
	var resp slackgo.SlackErrorResponse
	var limited *slackgo.RateLimitedError
	return errors.As(err, &resp) || errors.As(err, &limited)
}

func (s *stream) Append(ctx context.Context, md string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := s.esc
	text := s.esc.text(md, maxMarkdown, false)
	if text == "" { // all of it is held back until the rest of its line comes
		return nil
	}
	unsure, err := s.text(ctx, text, func(m StreamMode) error {
		return s.p.c.AppendStream(ctx, s.ref.Channel, s.ref.ID, text, m)
	})
	s.settle(saved, unsure, err)
	return notStreaming(err)
}

// settle keeps the reading of the stream as what Slack has, after a call that carried a text. If every call made was
// answered with an error, the text was not applied, and the caller may send it again, so the reading goes back to before
// it. If any call made was lost with no answer (unsure), the text may have been applied, and Slack may be in a block
// that Agent Kourier is not, or hold the text twice after a second try: the stream goes back to before the text too, and is
// blind from then on, which escapes everything, held-back text included.
func (s *stream) settle(saved mdText, unsure bool, err error) {
	if err == nil {
		return
	}
	s.esc = saved
	s.esc.blind = saved.blind || unsure
}

// noResult is what the card of a step says when the stream ended with no result for it. Slack has no state for a
// step whose outcome is not known, so the card is complete, and says so in words of Agent Kourier's own: a tick alone
// would read as success. It is the only text of a step that is not the agent's.
const noResult = "No result was reported."

// taskStatus is the status of a card for a step in the state given, and the output it shows, which is noResult for a
// step that ended with no outcome and nothing for any other. A stream's task_update and a message's task_card show a
// step alike.
func taskStatus(s chat.StepState) (slackgo.TaskCardStatus, string) {
	switch s {
	case chat.StepDone:
		return slackgo.TaskCardStatusComplete, ""
	case chat.StepFailed:
		return slackgo.TaskCardStatusError, ""
	case chat.StepEnded:
		return slackgo.TaskCardStatusComplete, noResult
	}
	return slackgo.TaskCardStatusInProgress, ""
}

// stepTitle is the title of a card: the step's title as stepText has it, or "(blank)" if nothing is left of it, as a
// button's label is.
func stepTitle(s string) string {
	return cmp.Or(stepText(s, caps.Limit(chat.WhereStepTitle)), "(blank)")
}

// Step shows the progress of one unit of the agent's work as a task_update chunk, which Slack shows as a card. The
// title and the details are untrusted text of one line, cleaned, escaped as markdown is (a task's text is not known to
// be plain, so nothing in it may be a link or a mention if it is rendered as markdown) and cut at the cap of a task's
// text, and a title that is nothing once cleaned is shown as blank, as a button's label is.
//
// Slack merges the chunks of one ID into one card (AppendStep): the title and the status replace the card's, and the
// details and the output are appended to it. So every chunk carries the title and the status, and the details and the
// output go in the first chunk that has them, and in none after: a step moved to its outcome keeps the details it was
// begun with. A chunk that Slack refused was not applied, and its details and output go with the next chunk of the
// step; one whose call was lost may have been, and they do not go again, for a card that shows them twice is worse
// than one that lacks them.
func (s *stream) Step(ctx context.Context, st chat.Step) error {
	if s.streamMode() == MarkdownMode {
		return chat.ErrNoSteps // a stream in the markdown_text mode cannot take a task_update chunk
	}
	if st.ID == "" {
		return fmt.Errorf("%w: a step with no ID", chat.ErrInvalid)
	}
	u := TaskUpdate{ID: st.ID, Title: stepTitle(st.Title)}
	status, output := taskStatus(st.State)
	u.Status = string(status)
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.shown[st.ID]
	if !was.details {
		u.Details = stepText(st.Detail, caps.Limit(chat.WhereStepDetail))
	}
	if !was.output {
		u.Output = output
	}
	err := s.p.c.AppendStep(ctx, s.ref.Channel, s.ref.ID, u)
	if err == nil || !refused(err) {
		if s.shown == nil {
			s.shown = map[string]card{}
		}
		s.shown[st.ID] = card{details: was.details || u.Details != "", output: was.output || u.Output != ""}
	}
	return notStreaming(err)
}

// Stop adds the last markdown, if any, and ends the stream, tagged with tag, in one call. What was streamed stays
// as the message's content, and final's blocks are appended under it: Slack does not replace the streamed text
// with them, so a final that repeats it shows it twice (dogfood, 2026-10-03). final's own fallback is not sent,
// since a stream has no separate text. A zero final sends no blocks, and leaves the message as it was streamed.
func (s *stream) Stop(ctx context.Context, md string, final chat.Message, tag string) error {
	var blocks json.RawMessage
	if final.Fallback != "" || len(final.Parts) > 0 {
		c, err := renderMessage(final)
		if err != nil {
			return err
		}
		blocks = c.Blocks
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := s.esc
	text := s.esc.text(md, maxMarkdown, true)
	unsure, err := s.text(ctx, text, func(m StreamMode) error {
		return s.p.c.StopStream(ctx, s.ref.Channel, s.ref.ID, text, blocks, tag, m)
	})
	s.settle(saved, unsure, err)
	return notStreaming(err)
}

// StreamAt is the stream that is the message at r. Its mode is the platform's, and if the stream of the earlier run
// that started it was in the other, the first text it is sent finds out (stream.text). Agent Kourier cannot know whether
// Slack is inside a block of the earlier run's, so the stream is blind: it escapes everything, for good (mdText).
func (p *Platform) StreamAt(r chat.Ref) chat.Stream {
	s := &stream{p: p, ref: r, esc: mdText{blind: true}}
	s.markdown.Store(p.streamMode() == MarkdownMode)
	return s
}

// notStreaming marks the error of a message that is not streaming, which Slack says by the code of its response:
// it ended, or it never was a stream (an update-path message's ts). Matched by the code, never by text.
// UNCONFIRMED(WP-04): the code real Slack gives for a ts that was never a stream. The fake gives
// message_not_in_streaming_state for both cases, so that is all that is tolerated.
func notStreaming(err error) error {
	var resp slackgo.SlackErrorResponse
	if errors.As(err, &resp) && resp.Err == "message_not_in_streaming_state" {
		return fmt.Errorf("%w: %w", chat.ErrNotStreaming, err)
	}
	return err
}
