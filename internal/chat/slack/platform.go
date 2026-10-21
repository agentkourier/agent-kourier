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

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
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

	// statusOff is latched when Slack refused the assistant status for good (MarkBusy): the platform makes no more calls for
	// it. statusWarned makes the log of it one.
	statusOff    atomic.Bool
	statusWarned sync.Once
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
	IsInAnyUserGroup(ctx context.Context, groups []string, user string) (bool, error)
	IsBot(ctx context.Context, user string) (bool, error)
	ResolveUser(ctx context.Context, id string) (dialect.User, error)
}

var _ API = (*Client)(nil)

// Render is o as the Slack message that Post sends: its text and blocks. Nothing in Agent Kourier delivers through it (the
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
	return in.sink.Message(ctx, InboundOf(in.p.conn, m))
}

// InboundOf is m as the neutral model has it, on the connection conn ("namespace/name"). A bot's message is flattened
// from its first legacy attachment into the neutral fields: the attachment's title is the Title, its title_link the
// Link, its text the Body, its footer the Footer and its fallback the Fallback; the message's top-level text stays Text. The sender is the message's bot_id, of
// the kind bot. The live path and the
// history (Client.History) both make an Inbound this way, and it is exported so that a test of a trigger can make
// the Inbound the intake would have been given for a message, without copying the conversion.
func InboundOf(conn string, m Message) chat.Inbound {
	msg := chat.Inbound{
		At:   chat.Ref{Platform: PlatformName, Conn: conn, Channel: m.Channel, Thread: m.ThreadTS, ID: m.TS},
		User: m.User, Text: m.Text, Mentioned: m.Mentioned, Direct: m.ChannelType == "im",
	}
	if m.Bot.ID != "" {
		msg.Bot = &chat.BotMessage{ID: m.Bot.ID, Kind: v1alpha1.SenderKindBot, Title: m.Bot.Title, Link: m.Bot.TitleLink,
			Fallback: m.Bot.Fallback, Footer: m.Bot.Footer, Body: m.Bot.Text}
	}
	return msg
}

// AcceptsBot is BotFilter: the neutral sink's answer, and no if it gives none (chat.BotFilter), so that a sink that
// knows nothing of bots is given none of their messages.
func (in inbound) AcceptsBot(channel, botID string) bool {
	f, ok := in.sink.(chat.BotFilter)
	return ok && f.AcceptsBot(channel, botID)
}

var _ BotFilter = inbound{}

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

// threadReader is what ThreadBetween asks of the API: the *Client has it, and a double of it may not.
type threadReader interface {
	ThreadBetween(ctx context.Context, conn, channel, threadTS, after, before string, limit int) ([]chat.HistoryMessage, int, error)
}

var _ chat.ThreadReader = (*Platform)(nil)

// ThreadBetween implements chat.ThreadReader over the Client's (Client.ThreadBetween). An API that cannot read a thread
// is chat.ErrNoThreadReads. A target that names no thread is chat.ErrInvalid.
func (p *Platform) ThreadBetween(ctx context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	r, ok := p.c.(threadReader)
	if !ok {
		return nil, 0, chat.ErrNoThreadReads
	}
	return r.ThreadBetween(ctx, p.conn, t.Channel, t.Thread, after, before, limit)
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

// IsInAnyGroup says whether user is in any of the Slack user groups with the IDs groups; it fails closed.
func (p *Platform) IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error) {
	return p.c.IsInAnyUserGroup(ctx, groups, user)
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

// Fits says whether the prompt fits in one message: 50 blocks, and for a prompt of tools also the limits of a
// tool's name and arguments and of the arguments of all its tools together (renderToolPrompt).
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

// interaction is a press as the neutral model has it. The pressed button's action ID, in the block that holds it, says
// which button it was. The adapter renders two blocks of buttons for a prompt: decide, for one tool, holds Approve and
// Deny, and submit holds Submit and, for several tools, Approve all and Deny all. Approve and Approve all are
// ActionApproveAll, Deny and Deny all are ActionDenyAll, and they ignore the controls, so a press of them carries no
// answers. Submit is the one button whose controls are read: the answers are read from the state of the controls, by
// the block and action IDs that renderPrompt made, and each only as the control it was asked in. The radio of a tool
// is read like a question's, and its value is the index of the option, "0" for Approve and "1" for Deny
// (chat.ToolApproveValue, chat.ToolDenyValue).
//
// A button the adapter does not render in that block makes no action and carries no answers, so that it can never be
// read as a decision. In a block the adapter renders for a prompt (submit, decide) it names no prompt (PromptID empty),
// which interact.Pipeline.Press and feedback.Handler.Press both refuse as not theirs, and the broker's handler drops,
// acknowledged: Submit in decide is such a button. In any other block, such as the block of a message part, it is any
// button, even when its action ID is one of the prompt's: its value is its own and is passed on for its handler to read
// (a feedback button is routed by it).
func (p *Platform) interaction(i Interaction) chat.Interaction {
	out := chat.Interaction{User: i.User, At: p.ref(i.Channel, i.ThreadTS, i.MessageTS), PromptID: i.Value}
	if i.BlockID != submitBlockID && i.BlockID != decideBlockID {
		return out
	}
	switch {
	case i.ActionID == approveAllActionID:
		out.Action = chat.ActionApproveAll
	case i.ActionID == denyAllActionID:
		out.Action = chat.ActionDenyAll
	case i.ActionID == submitActionID && i.BlockID == submitBlockID:
		out.Answers = answers(i.Inputs)
	default:
		out.PromptID = ""
	}
	return out
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
