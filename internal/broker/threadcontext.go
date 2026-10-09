package broker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// The thread context of a mentioned turn (spec 8.2, D38): in a Binding with chat.threadReplies: mention, the replies that
// people wrote in the thread since the session's previous turn, which were not turns themselves. The session manager asks for
// it (session.Config.ThreadContext) once for each turn it sends; this is what answers: read the thread through the platform,
// leave out what is not a person's unmentioned reply, name the authors, and render the block (render.ThreadRepliesCounted).
const (
	// threadFetchLimit is how many messages of the window the read asks for. The platform's limit and its count of what it
	// left out are of every message, Agent Kourier's own, other bots' and those that were turns included, so a window holds
	// Agent Kourier's own posts for the previous turn besides the people's replies: asking for only the newest few would let that
	// noise crowd the people out. It is the most that the Slack adapter reads of a window in any case (5 pages of 200); a
	// window longer than that is chat.ErrThreadTooLong.
	threadFetchLimit = 1000
	// threadRateWaitMax is the longest wait that a rate limit may ask for and still be waited out, once. A read that is
	// limited for longer, or again after the wait, is a read that failed.
	threadRateWaitMax = 5 * time.Second
	// threadReadFailed is what a turn carries when the earlier replies could not be read: fixed text, no content.
	threadReadFailed = "Agent Kourier could not read the earlier replies in this thread."
)

// The outcomes of agentkourier_thread_context_total.
const (
	threadRead        = "read"        // a block of replies went with the turn
	threadEmpty       = "empty"       // there was nothing to add
	threadCut         = "cut"         // a block went, and it says how many earlier replies it left out
	threadFailed      = "failed"      // the turn went with threadReadFailed
	threadUnsupported = "unsupported" // the platform cannot read a thread: the turn went with the mentioned message alone
)

// threadContextConfig configures a threadContext. Platform and Replies are required: newThreadContext refuses a config
// without them, naming the field. Logger and After are tunables with defaults.
type threadContextConfig struct {
	// Platform is the built chat platform, as the broker uses it (the fenced one, with an election). Whether it can read a
	// thread is asked of it by a type assertion on this value, as the trigger start asks for chat.Permalinker, and never of
	// builtPlatform, which hides it.
	Platform chat.Platform
	// Replies is the reply queue, which session.WereAdmitted reads to tell a reply that was a turn or an answer.
	Replies session.ReplyRecord
	Logger  *slog.Logger
	// After waits for a duration, and defaults to time.After. A test injects a clock.
	After func(time.Duration) <-chan time.Time
}

// threadContext implements session.Config.ThreadContext.
type threadContext struct {
	platform chat.Platform
	reader   chat.ThreadReader // nil if the platform does not implement it
	replies  session.ReplyRecord
	log      *slog.Logger
	after    func(time.Duration) <-chan time.Time

	mu    sync.Mutex
	noted map[string]bool // the Bindings that were told of an unsupported platform in the log
}

// newThreadContext returns a threadContext over cfg.
func newThreadContext(cfg threadContextConfig) (*threadContext, error) {
	switch {
	case cfg.Platform == nil:
		return nil, errors.New("broker: a Platform is required for the thread context")
	case cfg.Replies == nil:
		return nil, errors.New("broker: Replies is required for the thread context: it asks whether a message was a turn")
	}
	t := &threadContext{platform: cfg.Platform, replies: cfg.Replies, log: cfg.Logger, after: cfg.After, noted: map[string]bool{}}
	t.reader, _ = cfg.Platform.(chat.ThreadReader)
	if t.log == nil {
		t.log = slog.Default()
	}
	if t.after == nil {
		t.after = time.After
	}
	return t, nil
}

// Section is session.Config.ThreadContext: the section of the replies in the thread key between the messages after and
// before (both exclusive) for a mentioned turn of binding, or the fixed line that says they could not be read, or "". It
// never fails, and it logs no content (DP-2): a failure is counted and logged by Binding and error alone.
func (t *threadContext) Section(ctx context.Context, key session.Key, binding, after, before string) string {
	section, outcome := t.build(ctx, key, binding, after, before)
	telemetry.Count(ctx, "agentkourier_thread_context_total", binding, outcome)
	return section
}

func (t *threadContext) build(ctx context.Context, key session.Key, binding, after, before string) (section, outcome string) {
	if after == before { // nothing lies between a message and itself; an inverted window is the platform's to say (chat.ThreadReader)
		return "", threadEmpty
	}
	if t.reader == nil {
		return "", t.unsupported(ctx, binding)
	}
	target := chat.Target{Conn: key.Connection, Channel: key.Channel, Thread: key.Thread}
	msgs, dropped, err := t.read(ctx, target, after, before)
	switch {
	case errors.Is(err, chat.ErrNoThreadReads):
		return "", t.unsupported(ctx, binding)
	case err != nil:
		return t.failed(ctx, binding, "read the thread", err)
	}
	people, err := t.eligible(ctx, msgs)
	if err != nil {
		return t.failed(ctx, binding, "ask which messages were turns", err)
	}
	if over := len(people) - render.MaxThreadReplies; over > 0 { // the older ones are cut whatever their names; do not look them up
		dropped, people = dropped+over, people[over:]
	}
	section, leftOut := render.ThreadRepliesCounted(t.named(ctx, people), dropped)
	switch {
	case section == "":
		return "", threadEmpty
	case leftOut > 0:
		return section, threadCut
	}
	return section, threadRead
}

// read reads the window, and once more if the platform said to wait a few seconds: a rate limit that asks for up to
// threadRateWaitMax is waited out once; any other error, a longer wait, or a second refusal is the caller's to do without.
func (t *threadContext) read(ctx context.Context, target chat.Target, after, before string) ([]chat.HistoryMessage, int, error) {
	msgs, dropped, err := t.reader.ThreadBetween(ctx, target, after, before, threadFetchLimit)
	if err == nil || errors.Is(err, chat.ErrNoThreadReads) || errors.Is(err, chat.ErrThreadTooLong) {
		return msgs, dropped, err
	}
	wait := t.platform.Classify(chat.OpOther, err).RetryAfter
	if wait <= 0 || wait > threadRateWaitMax {
		return nil, 0, err
	}
	select {
	case <-t.after(wait):
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	return t.reader.ThreadBetween(ctx, target, after, before, threadFetchLimit)
}

// eligible returns the replies of msgs that the context shows, oldest first: a person's message that the adapter would
// forward, which is not Agent Kourier's own (its answers and notes), not a bot's (D36), has a user, has text once normalized (a
// reply of zero-width characters is blank to the block, and must not take one of its newest fifty places or cost a lookup)
// and was not itself a turn or an answer (the reply queue holds it, queued or delivered), so the agent is not told what it
// has already been sent. The checks that need no lookup come first, and the reply queue is asked once, about what is left.
func (t *threadContext) eligible(ctx context.Context, msgs []chat.HistoryMessage) ([]chat.HistoryMessage, error) {
	var people []chat.HistoryMessage
	var ids []string
	for _, m := range msgs {
		if m.Own || m.Ignored || m.Message.Bot != nil || m.Message.User == "" || strings.TrimSpace(redact.Normalize(m.Message.Text)) == "" {
			continue
		}
		people = append(people, m)
		ids = append(ids, chat.MessageID(m.Message.At))
	}
	if len(people) == 0 {
		return nil, nil
	}
	admitted, err := session.WereAdmitted(ctx, t.replies, ids)
	if err != nil {
		return nil, err
	}
	var out []chat.HistoryMessage
	for i, m := range people {
		if !admitted[ids[i]] {
			out = append(out, m)
		}
	}
	return out, nil
}

// named returns msgs as the replies the render shows, with the names the platform gives their authors (its user cache
// answers a person who wrote more than once). A name that cannot be had is left out: the reply shows the ID alone.
func (t *threadContext) named(ctx context.Context, msgs []chat.HistoryMessage) []render.ThreadReply {
	names := map[string]string{}
	out := make([]render.ThreadReply, len(msgs))
	for i, m := range msgs {
		id := m.Message.User
		name, ok := names[id]
		if !ok {
			if u, err := t.platform.ResolveUser(ctx, id); err == nil {
				name = u.Name
			}
			names[id] = name
		}
		out[i] = render.ThreadReply{UserID: id, Name: name, Text: m.Message.Text}
	}
	return out
}

// failed counts and logs a read that did not work, with no content, and returns the fixed line for the turn.
func (t *threadContext) failed(ctx context.Context, binding, step string, err error) (string, string) {
	t.log.WarnContext(ctx, "thread context: the turn goes without the earlier replies, with a line that says so",
		"binding", binding, "step", step, "error", err)
	return threadReadFailed, threadFailed
}

// unsupported logs, once for each Binding, that its mentioned turns go with their own message alone.
func (t *threadContext) unsupported(ctx context.Context, binding string) string {
	t.mu.Lock()
	first := !t.noted[binding]
	t.noted[binding] = true
	t.mu.Unlock()
	if first {
		t.log.InfoContext(ctx, "thread context: the chat platform cannot read a thread, so a mentioned turn carries its own message alone",
			"binding", binding)
	}
	return threadUnsupported
}
