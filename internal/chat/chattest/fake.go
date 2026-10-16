package chattest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// FakeError is an error of the fake whose classification the test chooses.
type FakeError struct {
	Verdict chat.Verdict
	Msg     string
}

func (e *FakeError) Error() string { return "fake platform: " + e.Msg }

// Message is a message the fake holds: what was posted, where, and what became of it.
type Message struct {
	Ref chat.Ref
	Out chat.Outgoing
	// Tag is what the message was posted with, for Find.
	Tag string
	// Updates is how many times it was replaced, and Reactions the reactions on it.
	Updates   int
	Reactions []string
	// Streamed is the markdown of a message that is, or was, a stream. A stopped stream keeps it, and Out, which
	// Stop sets to the final message, is shown under it: the stop adds to the stream and does not replace it.
	Streamed  string
	Streaming bool
	// Steps are the progress steps of a stream, in the order each was begun, each in the state it was last given.
	Steps []chat.Step
	// Recipient is the user a stream is shown to.
	Recipient string
	Posted    time.Time
}

// Ephemeral is a message the fake showed one person alone.
type Ephemeral struct {
	Target chat.Target
	User   string
	Msg    chat.Message
}

// FakePlatform is an in-memory chat.Platform for the tests of code that uses one. It keeps everything it
// is asked to do, and it is itself a faithful adapter of an imaginary platform with no markup: it shows
// text as the characters it is made of (chat.Literal), so it passes Conformance, which is how its own
// tests show that the suite accepts a safe platform.
type FakePlatform struct {
	// MaxQuestions is how many questions a prompt may have before Fits says it is too large (default 20).
	MaxQuestions int
	// MaxTools is how many tools a prompt may have before Fits says it is too large (default 20).
	MaxTools int
	// Now is the clock (time.Now by default).
	Now func() time.Time

	mu         sync.Mutex
	caps       chat.Caps
	messages   []*Message
	byRef      map[chat.Ref]*Message // the index of messages, which find reads
	ephemerals []Ephemeral
	members    map[string][]string // channel to users
	groups     map[string][]string // group ID to users
	users      map[string]chat.User
	bots       map[string]bool
	failures   map[string][]error // method to the errors the next calls return
	sink       chat.Sink
	ready      chan struct{}
	runErr     error
	sent       []Shown        // what was received since Sent was last called
	said       []chat.Inbound // what Say delivered, which ThreadBetween reads with what was posted
}

// NewFake returns a fake with every capability, and limits like a real platform's.
func NewFake() *FakePlatform {
	return &FakePlatform{
		MaxQuestions: 20, MaxTools: 20, Now: time.Now,
		caps: chat.Caps{
			Streams: true, Steps: true, Ephemeral: true, Edits: true, Inputs: true, MaxText: 4000, AckReaction: "hourglass",
			Limits: map[chat.Where]int{chat.WhereHeader: 150, chat.WhereButton: 75, chat.WhereChoice: 75, chat.WhereHint: 150, chat.WhereBody: 12000, chat.WhereStream: 12000},
		},
		byRef: map[chat.Ref]*Message{}, members: map[string][]string{}, groups: map[string][]string{}, users: map[string]chat.User{}, bots: map[string]bool{}, failures: map[string][]error{}, ready: make(chan struct{}),
	}
}

// SetCaps replaces what the fake says it can do.
func (f *FakePlatform) SetCaps(c chat.Caps) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps = c
}

// FailNext makes the next calls of method (its name, such as "Post") return errs in turn.
func (f *FakePlatform) FailNext(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = append(f.failures[method], errs...)
}

// FailRun makes Run return err once it is started.
func (f *FakePlatform) FailRun(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runErr = err
}

// AddUser tells the fake of a person, and SetMembers who is in a channel. A user the fake was told nothing
// of is a bot, with an error: it fails closed.
func (f *FakePlatform) AddUser(u chat.User, bot bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID], f.bots[u.ID] = u, bot
}

// SetMembers sets who is in channel.
func (f *FakePlatform) SetMembers(channel string, users ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[channel] = users
}

// SetGroup sets who is in the group with the platform ID id. A group the fake was not told of is unreadable: IsInAnyGroup
// is false, with an error that wraps chat.ErrGroupsMisconfigured, as a platform's answer for a group that does not exist
// does. FailNext("IsInAnyGroup", errors.New(...)) makes the other kind, an error that may pass on a retry, and
// FailNext with an error that wraps chat.ErrGroupsMisconfigured makes the first for a group that is set.
func (f *FakePlatform) SetGroup(id string, users ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[id] = users
}

// Messages returns what is posted, oldest first, as copies.
func (f *FakePlatform) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.messages))
	for i, m := range f.messages {
		out[i] = *m
		out[i].Reactions = slices.Clone(m.Reactions)
		out[i].Steps = slices.Clone(m.Steps)
	}
	return out
}

// Ephemerals returns what was shown to one person alone.
func (f *FakePlatform) Ephemerals() []Ephemeral {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ephemerals)
}

// fail returns the error queued for method, if there is one.
func (f *FakePlatform) fail(method string) error {
	errs := f.failures[method]
	if len(errs) == 0 {
		return nil
	}
	f.failures[method] = errs[1:]
	return errs[0]
}

func (f *FakePlatform) find(r chat.Ref) *Message { return f.byRef[r] }

// keep adds m to what the fake holds.
func (f *FakePlatform) keep(m *Message) {
	f.messages = append(f.messages, m)
	f.byRef[m.Ref] = m
}

// Run implements chat.Platform: it holds the connection until ctx ends, delivering what Deliver and Press
// hand it.
func (f *FakePlatform) Run(ctx context.Context, sink chat.Sink) error {
	f.mu.Lock()
	if err := f.fail("Run"); err != nil {
		f.mu.Unlock()
		return err
	}
	f.sink = sink
	runErr := f.runErr
	select {
	case <-f.ready:
	default:
		close(f.ready)
	}
	f.mu.Unlock()
	if runErr != nil {
		return runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

// Say delivers a message from a person to the sink, once Run has started, and returns the sink's error. The fake keeps it
// in the conversation, whatever the sink says, as a platform does: ThreadBetween reads it.
func (f *FakePlatform) Say(ctx context.Context, m chat.Inbound) error {
	sink, err := f.running(ctx)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.said = append(f.said, m)
	f.mu.Unlock()
	return sink.Message(ctx, m)
}

// Press delivers a press of a button to the sink, once Run has started, and returns the sink's error.
func (f *FakePlatform) Press(ctx context.Context, i chat.Interaction) error {
	sink, err := f.running(ctx)
	if err != nil {
		return err
	}
	return sink.Interaction(ctx, i)
}

func (f *FakePlatform) running(ctx context.Context) (chat.Sink, error) {
	select {
	case <-f.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sink, nil
}

// Post implements chat.Platform.
func (f *FakePlatform) Post(_ context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Post"); err != nil {
		return chat.Ref{}, err
	}
	if err := f.accept(o); err != nil {
		return chat.Ref{}, err
	}
	ref := chat.Ref{Platform: "fake", Conn: t.Conn, Channel: t.Channel, Thread: t.Thread, ID: fmt.Sprintf("%d.000", len(f.messages)+1)}
	f.keep(&Message{Ref: ref, Out: o, Tag: tag, Posted: f.Now()})
	f.sent = append(f.sent, f.show(o)...)
	return ref, nil
}

// accept refuses what a platform would: a malformed Outgoing, and a prompt that is too large.
func (f *FakePlatform) accept(o chat.Outgoing) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Prompt != nil {
		return f.fits(*o.Prompt)
	}
	return nil
}

// Find implements chat.Platform.
func (f *FakePlatform) Find(_ context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Find"); err != nil {
		return chat.Ref{}, false, err
	}
	for _, m := range f.messages {
		if m.Tag == tag && m.Ref.Channel == t.Channel && !m.Posted.Before(since) {
			return m.Ref, true, nil
		}
	}
	return chat.Ref{}, false, nil
}

// Update implements chat.Platform. A message that is not there is a permanent error.
func (f *FakePlatform) Update(_ context.Context, r chat.Ref, o chat.Outgoing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Update"); err != nil {
		return err
	}
	if err := f.accept(o); err != nil {
		return err
	}
	m := f.find(r)
	if m == nil {
		return &FakeError{Verdict: chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "message_not_found"}, Msg: "no such message"}
	}
	m.Out = o
	m.Updates++
	f.sent = append(f.sent, f.show(o)...)
	return nil
}

type fakeStream struct {
	f   *FakePlatform
	ref chat.Ref
}

// Stream implements chat.Platform.
func (f *FakePlatform) Stream(_ context.Context, t chat.Target, to, markdown string) (chat.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.caps.Streams {
		return nil, chat.ErrNoStreams
	}
	if err := f.fail("Stream"); err != nil {
		return nil, err
	}
	ref := chat.Ref{Platform: "fake", Conn: t.Conn, Channel: t.Channel, Thread: t.Thread, ID: fmt.Sprintf("%d.000", len(f.messages)+1)}
	f.keep(&Message{Ref: ref, Streamed: markdown, Streaming: true, Recipient: to, Posted: f.Now()})
	f.sent = append(f.sent, f.showMarkdown(markdown))
	return &fakeStream{f, ref}, nil
}

func (s *fakeStream) Ref() chat.Ref { return s.ref }

// StreamAt implements chat.Platform.
func (f *FakePlatform) StreamAt(r chat.Ref) chat.Stream { return &fakeStream{f, r} }

func errNotStreaming() error {
	return fmt.Errorf("%w: %w", chat.ErrNotStreaming, &FakeError{Verdict: chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "not_streaming"}, Msg: "the message is not streaming"})
}

func (s *fakeStream) Append(_ context.Context, markdown string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.fail("Append"); err != nil {
		return err
	}
	m := s.f.find(s.ref)
	if m == nil || !m.Streaming {
		return errNotStreaming()
	}
	m.Streamed += markdown
	s.f.sent = append(s.f.sent, s.f.showMarkdown(markdown))
	return nil
}

// Step implements chat.Stream: a step the stream has not seen is added, and one it has seen takes the new state. A
// fake whose caps say no Steps shows none.
func (s *fakeStream) Step(_ context.Context, st chat.Step) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if !s.f.caps.Steps {
		return chat.ErrNoSteps
	}
	if err := s.f.fail("Step"); err != nil {
		return err
	}
	m := s.f.find(s.ref)
	if m == nil || !m.Streaming {
		return errNotStreaming()
	}
	if i := slices.IndexFunc(m.Steps, func(have chat.Step) bool { return have.ID == st.ID }); i >= 0 {
		m.Steps[i] = st
	} else {
		m.Steps = append(m.Steps, st)
	}
	s.f.sent = append(s.f.sent, s.f.literal(st.Title, chat.WhereStepTitle))
	if st.Detail != "" {
		s.f.sent = append(s.f.sent, s.f.literal(st.Detail, chat.WhereStepDetail))
	}
	return nil
}

// Stop implements chat.Stream. The final message is kept as Out, under what was streamed.
func (s *fakeStream) Stop(_ context.Context, markdown string, final chat.Message, tag string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.fail("Stop"); err != nil {
		return err
	}
	m := s.f.find(s.ref)
	if m == nil || !m.Streaming {
		return errNotStreaming()
	}
	if markdown != "" {
		m.Streamed += markdown
		s.f.sent = append(s.f.sent, s.f.showMarkdown(markdown))
	}
	m.Streaming, m.Out, m.Tag = false, chat.Say(final), tag
	s.f.sent = append(s.f.sent, s.f.show(m.Out)...)
	return nil
}

// Ephemeral implements chat.Platform.
func (f *FakePlatform) Ephemeral(_ context.Context, t chat.Target, user string, m chat.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Ephemeral"); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	f.ephemerals = append(f.ephemerals, Ephemeral{Target: t, User: user, Msg: m})
	f.sent = append(f.sent, f.show(chat.Say(m))...)
	return nil
}

// RefAt implements chat.Platform.
func (f *FakePlatform) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: "fake", Channel: channel, Thread: thread, ID: id}
}

// React implements chat.Platform. A reaction that is there already is success.
func (f *FakePlatform) React(_ context.Context, r chat.Ref, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("React"); err != nil {
		return err
	}
	m := f.find(r)
	if m == nil {
		return &FakeError{Verdict: chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "message_not_found"}, Msg: "no such message"}
	}
	if !slices.Contains(m.Reactions, name) {
		m.Reactions = append(m.Reactions, name)
	}
	return nil
}

// Unreact implements chat.Platform. A reaction that is not there is success.
func (f *FakePlatform) Unreact(_ context.Context, r chat.Ref, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Unreact"); err != nil {
		return err
	}
	m := f.find(r)
	if m == nil {
		return &FakeError{Verdict: chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "message_not_found"}, Msg: "no such message"}
	}
	m.Reactions = slices.DeleteFunc(m.Reactions, func(n string) bool { return n == name })
	return nil
}

var _ chat.ThreadReader = (*FakePlatform)(nil)

// ThreadBetween implements chat.ThreadReader over what the fake holds of the thread: what was posted in it (Agent Kourier's own,
// Own) and what Say delivered to it, by their IDs, which it compares as a platform's timestamps ("seconds.fraction"; the
// IDs of its own posts are "n.000"). The window is exclusive on both sides, an empty bound is none, and the thread's root is
// no reply. It has no page cap, so it never returns chat.ErrThreadTooLong. A message posted as a stream is its streamed text.
func (f *FakePlatform) ThreadBetween(_ context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ThreadBetween"); err != nil {
		return nil, 0, err
	}
	if t.Thread == "" || limit < 1 {
		return nil, 0, fmt.Errorf("fake platform: %w: a thread to read and a count of at least 1", chat.ErrInvalid)
	}
	if after != "" && before != "" && cmpID(after, before) >= 0 {
		return nil, 0, nil
	}
	in := func(r chat.Ref) bool {
		return r.Channel == t.Channel && r.Thread == t.Thread && r.ID != t.Thread &&
			(after == "" || cmpID(r.ID, after) > 0) && (before == "" || cmpID(r.ID, before) < 0)
	}
	var out []chat.HistoryMessage
	for _, m := range f.messages {
		if in(m.Ref) {
			text := m.Out.Message.Fallback
			if m.Streamed != "" {
				text = m.Streamed
			}
			out = append(out, chat.HistoryMessage{Message: chat.Inbound{At: m.Ref, Text: text}, Own: true})
		}
	}
	for _, m := range f.said {
		if in(m.At) {
			out = append(out, chat.HistoryMessage{Message: m})
		}
	}
	slices.SortStableFunc(out, func(a, b chat.HistoryMessage) int { return cmpID(a.Message.At.ID, b.Message.At.ID) })
	dropped := max(len(out)-limit, 0)
	return slices.Clone(out[dropped:]), dropped, nil
}

// cmpID orders two message IDs of the form "seconds.fraction", as Slack orders its timestamps: by the length of the
// seconds, then the digits, then the fraction.
func cmpID(a, b string) int {
	as, af, _ := strings.Cut(a, ".")
	bs, bf, _ := strings.Cut(b, ".")
	pad := func(f string) string { return f + strings.Repeat("0", max(6-len(f), 0)) }
	return cmp.Or(cmp.Compare(len(as), len(bs)), strings.Compare(as, bs), strings.Compare(pad(af), pad(bf)))
}

// IsMember implements chat.Platform.
func (f *FakePlatform) IsMember(_ context.Context, channel, user string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("IsMember"); err != nil {
		return false, err
	}
	return slices.Contains(f.members[channel], user), nil
}

// IsInAnyGroup implements chat.Platform: groups are read in order, up to the first that lists user, and a group the fake
// was not told of is an error that ends the call and wraps chat.ErrGroupsMisconfigured.
func (f *FakePlatform) IsInAnyGroup(_ context.Context, groups []string, user string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(groups) == 0 || user == "" {
		return false, nil
	}
	if err := f.fail("IsInAnyGroup"); err != nil {
		return false, err
	}
	for _, g := range groups {
		members, ok := f.groups[g]
		if !ok {
			return false, fmt.Errorf("fake platform: no such group %q: %w", g, chat.ErrGroupsMisconfigured)
		}
		if slices.Contains(members, user) {
			return true, nil
		}
	}
	return false, nil
}

// IsBot implements chat.Platform: a user the fake was not told of is a bot, with an error.
func (f *FakePlatform) IsBot(_ context.Context, user string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("IsBot"); err != nil {
		return true, err
	}
	if _, ok := f.users[user]; !ok {
		return true, fmt.Errorf("fake platform: no such user %q", user)
	}
	return f.bots[user], nil
}

// ResolveUser implements chat.Platform.
func (f *FakePlatform) ResolveUser(_ context.Context, id string) (chat.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ResolveUser"); err != nil {
		return chat.User{}, err
	}
	u, ok := f.users[id]
	if !ok {
		return chat.User{}, fmt.Errorf("fake platform: no such user %q", id)
	}
	return u, nil
}

// Fits implements chat.Platform: a prompt of more than MaxQuestions questions, or of more than MaxTools tools, is
// too large, and so is one with a tool whose name or arguments, cleaned, are longer than the limit of their place:
// the text of a tool awaiting approval is refused and never shown cut.
func (f *FakePlatform) Fits(p chat.Prompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fits(p)
}

func (f *FakePlatform) fits(p chat.Prompt) error {
	if len(p.Questions) > f.MaxQuestions {
		return fmt.Errorf("%w: %d questions, at most %d", chat.ErrTooLarge, len(p.Questions), f.MaxQuestions)
	}
	if len(p.Tools) > f.MaxTools {
		return fmt.Errorf("%w: %d tools, at most %d", chat.ErrTooLarge, len(p.Tools), f.MaxTools)
	}
	for i, t := range p.Tools {
		if n, lim := chat.Units(chat.Clean(t.Name, chat.WhereToolName.Line())), f.caps.Limit(chat.WhereToolName); n > lim {
			return fmt.Errorf("%w: the name of tool %d is %d units, at most %d", chat.ErrTooLarge, i, n, lim)
		}
		if n, lim := chat.Units(chat.Clean(t.Args, chat.WhereToolArgs.Line())), f.caps.Limit(chat.WhereToolArgs); n > lim {
			return fmt.Errorf("%w: the arguments of tool %d are %d units, at most %d", chat.ErrTooLarge, i, n, lim)
		}
	}
	return nil
}

// Classify implements chat.Platform: a FakeError says what it is, and any other error is transient.
func (f *FakePlatform) Classify(_ chat.Op, err error) chat.Verdict {
	var fe *FakeError
	if errors.As(err, &fe) {
		return fe.Verdict
	}
	return chat.Verdict{Class: chat.Transient}
}

// Capabilities implements chat.Platform.
func (f *FakePlatform) Capabilities() chat.Caps {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps
}

// Subject is the fake as a conformance subject: it shows every field as chat.Literal does, and acts on
// nothing, so Sent reports no construct. It shows tool approvals.
func (f *FakePlatform) Subject() Subject {
	return Subject{
		Platform: f,
		Sent: func() []Shown {
			f.mu.Lock()
			defer f.mu.Unlock()
			s := f.sent
			f.sent = nil
			return s
		},
		Inert:         func() error { return nil },
		ToolApprovals: true,
		TooLargeTools: &chat.Prompt{ID: "ix-big-tools", Tools: make([]chat.Tool, f.MaxTools+1)},
	}
}

// literal is s as the fake shows it at the place w.
func (f *FakePlatform) literal(s string, w chat.Where) Shown {
	return Shown{Displayed: chat.Literal(s, w, f.caps)}
}

func (f *FakePlatform) showMarkdown(md string) Shown { return f.literal(md, chat.WhereStream) }

// show is every field of o as the fake shows it. The caller holds f.mu.
func (f *FakePlatform) show(o chat.Outgoing) []Shown {
	var out []Shown
	for _, fld := range Fields(o) {
		out = append(out, f.literal(fld.Text, fld.Where))
	}
	return out
}
