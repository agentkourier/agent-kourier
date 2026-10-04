package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// PlatformName is the name Refs of Slack messages carry: the first part of chat.MessageID, which is how
// the store already names a Slack message.
const PlatformName = "slack"

// Platform is Slack as a chat.Platform: the Client's calls, with the neutral model rendered as Block Kit and
// every text neutralised for Slack (escape.go). It is the adapter of one ChatConnection. It is safe for
// concurrent use.
type Platform struct {
	c    API
	conn string
	log  *slog.Logger

	// markdown is latched when Slack refused the chunks mode and took the markdown_text one: every stream
	// from then on starts in the mode that works, with no failed call first. warned makes the log of it one.
	markdown atomic.Bool
	warned   sync.Once
}

// API is what the Platform asks of the Slack client: *Client, or a double of it.
type API interface {
	Run(ctx context.Context, sink Sink) error
	Post(ctx context.Context, channel, threadTS string, c Content, outboxID string) (string, error)
	Find(ctx context.Context, channel, threadTS, outboxID string, since time.Time) (ts string, found bool, err error)
	Update(ctx context.Context, channel, ts string, c Content) error
	StartStream(ctx context.Context, channel, threadTS, recipientUser, markdown string, mode StreamMode) (string, error)
	AppendStream(ctx context.Context, channel, ts, markdown string, mode StreamMode) error
	AppendStep(ctx context.Context, channel, ts string, u TaskUpdate) error
	StopStream(ctx context.Context, channel, ts, markdown string, blocks json.RawMessage, outboxID string, mode StreamMode) error
	PostEphemeral(ctx context.Context, channel, threadTS, user string, c Content) error
	React(ctx context.Context, channel, ts, name string) error
	Unreact(ctx context.Context, channel, ts, name string) error
	IsChannelMember(ctx context.Context, channel, user string) (bool, error)
	IsBot(ctx context.Context, user string) (bool, error)
	ResolveUser(ctx context.Context, id string) (dialect.User, error)
}

var _ API = (*Client)(nil)

// Render is o as the Slack message that Post sends: its text and blocks. Nothing in courier delivers through it (the
// outbox stores the neutral model and the Platform renders it at delivery); it is for a test or a tool that shows what
// Slack would be sent.
func Render(o chat.Outgoing) (Content, error) { return renderOutgoing(o) }

var _ chat.Platform = (*Platform)(nil)

// NewPlatform returns the adapter of the ChatConnection conn ("namespace/name"), which c is the client of.
func NewPlatform(c API, conn string) *Platform {
	return &Platform{c: c, conn: conn, log: slog.Default()}
}

// WithLogger sets the logger of the Platform, which logs the one thing it reports on its own: Slack's refusal of the
// chunks mode (Stream). It returns p, for the call that builds it.
func (p *Platform) WithLogger(l *slog.Logger) *Platform {
	if l != nil {
		p.log = l
	}
	return p
}

func (p *Platform) ref(channel, thread, ts string) chat.Ref {
	return chat.Ref{Platform: PlatformName, Conn: p.conn, Channel: channel, Thread: thread, ID: ts}
}

// RefAt implements chat.Platform: the Ref of the message with the ts id in a channel and thread.
func (p *Platform) RefAt(channel, thread, id string) chat.Ref { return p.ref(channel, thread, id) }

// Run connects over Socket Mode and gives sink what people write and press, until ctx ends or the connection
// fails for good (Client.Run).
func (p *Platform) Run(ctx context.Context, sink chat.Sink) error {
	return p.c.Run(ctx, inbound{p: p, sink: sink})
}

// inbound is the Sink of the Client, which hands the neutral sink what it decodes.
type inbound struct {
	p    *Platform
	sink chat.Sink
}

func (in inbound) Message(ctx context.Context, m Message) error {
	return in.sink.Message(ctx, chat.Inbound{
		At: in.p.ref(m.Channel, m.ThreadTS, m.TS), User: m.User, Text: m.Text,
		Mentioned: m.Mentioned, Direct: m.ChannelType == "im",
	})
}

func (in inbound) Interaction(ctx context.Context, i Interaction) error {
	return in.sink.Interaction(ctx, in.p.interaction(i))
}

// Post says o in t, tagged with tag, and returns the message.
func (p *Platform) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	c, err := renderOutgoing(o)
	if err != nil {
		return chat.Ref{}, err
	}
	ts, err := p.c.Post(ctx, t.Channel, t.Thread, c, tag)
	if err != nil {
		return chat.Ref{}, err
	}
	return p.ref(t.Channel, t.Thread, ts), nil
}

// Find returns the message in t tagged with tag, posted no earlier than about since.
func (p *Platform) Find(ctx context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error) {
	ts, found, err := p.c.Find(ctx, t.Channel, t.Thread, tag, since)
	if err != nil || !found {
		return chat.Ref{}, false, err
	}
	return p.ref(t.Channel, t.Thread, ts), true, nil
}

// Update replaces the message at r with o. Slack takes the blocks again with every update, so an update
// always sends them.
func (p *Platform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	c, err := renderOutgoing(o)
	if err != nil {
		return err
	}
	return p.c.Update(ctx, r.Channel, r.ID, c)
}

// Stream starts a streaming message in t, shown to the user to, with the markdown first. The stream starts in the
// chunks mode, which is the one that can show steps. That mode is a guess about Slack that no one has confirmed
// (UNCONFIRMED(WP-04)), and a Slack that refuses it, with one of the codes of refusesChunks, must not cost courier its
// replies: the start is made once more in the markdown_text mode, and if that works the platform stays in it for
// the process, so that the next stream starts there with no failed call first, and is logged once. A stream in that
// mode shows no steps. A start with no text carries no mode and cannot be refused for it.
func (p *Platform) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	mode := p.streamMode()
	text := markdown(md, maxMarkdown)
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
	s := &stream{p: p, ref: p.ref(t.Channel, t.Thread, ts)}
	s.mode.Store(int32(mode))
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
	p    *Platform
	ref  chat.Ref
	mode atomic.Int32 // the StreamMode of the stream, which a refusal of the chunks mode changes
}

func (s *stream) Ref() chat.Ref { return s.ref }

// text makes a call that carries text, in the mode of the stream. A stream that was started with no text has no mode
// yet, and the first text may be what Slack refuses in chunks: it is sent once more in markdown_text, and if that works
// the stream, and the platform, are in that mode from then on. One that is refused in both is no mode's trouble, and
// the first answer is kept.
func (s *stream) text(ctx context.Context, md string, call func(StreamMode) error) error {
	mode := StreamMode(s.mode.Load())
	err := call(mode)
	if err != nil && mode == ChunksMode && md != "" && refusesChunks(err) {
		if retry := call(MarkdownMode); retry == nil {
			s.mode.Store(int32(MarkdownMode))
			s.p.fallBack(ctx, err)
			return nil
		}
	}
	return err
}

func (s *stream) Append(ctx context.Context, md string) error {
	text := markdown(md, maxMarkdown)
	return notStreaming(s.text(ctx, text, func(m StreamMode) error {
		return s.p.c.AppendStream(ctx, s.ref.Channel, s.ref.ID, text, m)
	}))
}

// noResult is what the card of a step says when the stream ended with no result for it. Slack has no state for a
// step whose outcome is not known, so the card is complete, and says so in words of courier's own: a tick alone
// would read as success. It is the only text of a step that is not the agent's.
const noResult = "No result was reported."

// Step shows the progress of one unit of the agent's work as a task_update chunk, which Slack shows as a card. The
// title and the details are untrusted text of one line, cleaned, escaped as markdown is (a task's text is not known to
// be plain, so nothing in it may be a link or a mention if it is rendered as markdown) and cut at the cap of a task's
// text, and a title that is nothing once cleaned is shown as blank, as a button's label is.
//
// The chunk always carries the whole card, title and details with the status, so that it reads the same whether Slack
// replaces a task by a chunk or merges the two.
func (s *stream) Step(ctx context.Context, st chat.Step) error {
	if StreamMode(s.mode.Load()) == MarkdownMode {
		return chat.ErrNoSteps // a stream in the markdown_text mode cannot take a task_update chunk
	}
	if st.ID == "" {
		return fmt.Errorf("%w: a step with no ID", chat.ErrInvalid)
	}
	u := TaskUpdate{
		ID:      st.ID,
		Title:   cmp.Or(stepText(st.Title, caps.Limit(chat.WhereStepTitle)), "(blank)"),
		Details: stepText(st.Detail, caps.Limit(chat.WhereStepDetail)),
	}
	switch st.State {
	case chat.StepDone:
		u.Status = string(slackgo.TaskCardStatusComplete)
	case chat.StepFailed:
		u.Status = string(slackgo.TaskCardStatusError)
	case chat.StepEnded:
		u.Status, u.Output = string(slackgo.TaskCardStatusComplete), noResult
	default:
		u.Status = string(slackgo.TaskCardStatusInProgress)
	}
	return notStreaming(s.p.c.AppendStep(ctx, s.ref.Channel, s.ref.ID, u))
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
	text := markdown(md, maxMarkdown)
	return notStreaming(s.text(ctx, text, func(m StreamMode) error {
		return s.p.c.StopStream(ctx, s.ref.Channel, s.ref.ID, text, blocks, tag, m)
	}))
}

// StreamAt is the stream that is the message at r. Its mode is the platform's, and if the stream of the earlier run
// that started it was in the other, the first text it is sent finds out (stream.text).
func (p *Platform) StreamAt(r chat.Ref) chat.Stream {
	s := &stream{p: p, ref: r}
	s.mode.Store(int32(p.streamMode()))
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

// Ephemeral shows m to user alone.
func (p *Platform) Ephemeral(ctx context.Context, t chat.Target, user string, m chat.Message) error {
	c, err := renderMessage(m)
	if err != nil {
		return err
	}
	return p.c.PostEphemeral(ctx, t.Channel, t.Thread, user, c)
}

// React puts the reaction name on the message at r.
func (p *Platform) React(ctx context.Context, r chat.Ref, name string) error {
	return p.c.React(ctx, r.Channel, r.ID, name)
}

// Unreact takes the reaction name off the message at r.
func (p *Platform) Unreact(ctx context.Context, r chat.Ref, name string) error {
	return p.c.Unreact(ctx, r.Channel, r.ID, name)
}

// IsMember says whether user is in channel; it fails closed.
func (p *Platform) IsMember(ctx context.Context, channel, user string) (bool, error) {
	return p.c.IsChannelMember(ctx, channel, user)
}

// IsBot says whether user is a bot; it fails closed.
func (p *Platform) IsBot(ctx context.Context, user string) (bool, error) {
	return p.c.IsBot(ctx, user)
}

// ResolveUser returns the user with the Slack ID id.
func (p *Platform) ResolveUser(ctx context.Context, id string) (chat.User, error) {
	u, err := p.c.ResolveUser(ctx, id)
	if err != nil {
		return chat.User{}, err
	}
	return chat.User{ID: u.ID, Name: u.Name, Email: u.Email}, nil
}

// Fits says whether the prompt fits in one message, which is 50 blocks.
func (p *Platform) Fits(pr chat.Prompt) error {
	_, err := renderPrompt(pr)
	return err
}

// Capabilities is what Slack shows.
func (p *Platform) Capabilities() chat.Caps { return caps }

// Classify says what an error of the Client means for a retry: a rejected credential is fatal, the codes
// that say the same call will never work are permanent, with the reason in neutral terms and Slack's own
// code beside it, and anything else, a rate limit, a server error or a timeout, may pass.
func (p *Platform) Classify(op chat.Op, err error) chat.Verdict {
	if code := fatalAuthCode(err); code != "" {
		return chat.Verdict{Class: chat.FatalAuth, Code: code}
	}
	switch op {
	case chat.OpPost:
		if code := permanentCode(err); code != "" {
			return chat.Verdict{Class: chat.Permanent, Reason: postReason[code], Code: code}
		}
	case chat.OpReact:
		if code := reactionPermanentCode(err); code != "" {
			return chat.Verdict{Class: chat.Permanent, Reason: reactionReason[code], Code: code}
		}
	}
	v := chat.Verdict{Class: chat.Transient}
	if limited := new(*slackgo.RateLimitedError); errors.As(err, limited) {
		v.RetryAfter = max((*limited).RetryAfter, time.Second) // one that names no wait is still one
	}
	return v
}

// The neutral reason of each code permanentCode and reactionPermanentCode give. A code not here is Other.
var (
	postReason = map[string]chat.Reason{
		"channel_not_found": chat.ReasonChannelGone, "is_archived": chat.ReasonChannelGone, "not_in_channel": chat.ReasonChannelGone,
		"msg_too_long": chat.ReasonTooLong, "invalid_blocks": chat.ReasonInvalidContent, "invalid_blocks_format": chat.ReasonInvalidContent,
		"invalid_chunks": chat.ReasonInvalidContent, "streaming_mode_mismatch": chat.ReasonInvalidContent, "cannot_provide_both_markdown_text_and_chunks": chat.ReasonInvalidContent,
	}
	reactionReason = map[string]chat.Reason{
		"message_not_found": chat.ReasonChannelGone, "channel_not_found": chat.ReasonChannelGone, "is_archived": chat.ReasonChannelGone,
		"not_in_channel": chat.ReasonChannelGone, "no_item_specified": chat.ReasonInvalidContent, "bad_timestamp": chat.ReasonInvalidContent,
		"invalid_name":  chat.ReasonInvalidContent,
		"thread_locked": chat.ReasonOther, "not_reactable": chat.ReasonOther, "too_many_reactions": chat.ReasonOther, "too_many_emoji": chat.ReasonOther,
	}
)

// interaction is a press as the neutral model has it: the answers are read from the state of the controls, by
// the block and action IDs that renderPrompt made, and each only as the control it was asked in.
func (p *Platform) interaction(i Interaction) chat.Interaction {
	return chat.Interaction{
		User: i.User, At: p.ref(i.Channel, i.ThreadTS, i.MessageTS), PromptID: i.Value, Answers: answers(i.Inputs),
	}
}

var blockIndex = regexp.MustCompile(`^q(\d{1,4})$`)

// answers reads the answer of each question from the inputs. An element of another type than its action ID
// says, or an input that holds nothing, is an unanswered question: an answer is never one the person did not
// give.
func answers(inputs map[string]map[string]Input) []chat.Answer {
	var out []chat.Answer
	for block, actions := range inputs {
		m := blockIndex.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		q, _ := strconv.Atoi(m[1])
		for id, in := range actions {
			control, ok := controlOf(id, q)
			if !ok || controlElement[control] != in.Type || len(in.Values) == 0 {
				continue
			}
			values := slices.Clone(in.Values)
			if control == controlNumbers {
				values = positions(in.Values)
			}
			out = append(out, chat.Answer{Question: q, Values: values})
		}
	}
	slices.SortFunc(out, func(a, b chat.Answer) int { return cmp.Compare(a.Question, b.Question) })
	return out
}

// controlOf is the control of the action ID id of question q, or ok false if it is not one of that question.
func controlOf(id string, q int) (byte, bool) {
	if len(id) < 2 || id[1:] != strconv.Itoa(q) {
		return 0, false
	}
	_, ok := controlElement[id[0]]
	return id[0], ok
}

// positions turns the numbers a person typed beside a numbered list (1-based, separated by commas or
// spaces) into the indices the neutral model answers choices with. A token that is not a number stays as
// it was typed, for the caller to find that it is no choice.
func positions(typed []string) []string {
	var out []string
	for _, v := range typed {
		for _, tok := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			if n, err := strconv.Atoi(tok); err == nil && n >= 1 {
				out = append(out, strconv.Itoa(n-1))
			} else {
				out = append(out, tok)
			}
		}
	}
	return out
}
