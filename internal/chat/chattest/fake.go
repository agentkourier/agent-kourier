package chattest

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// Streamed is the markdown of a message that is, or was, a stream.
	Streamed  string
	Streaming bool
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
	// Now is the clock (time.Now by default).
	Now func() time.Time

	mu         sync.Mutex
	caps       chat.Caps
	messages   []*Message
	byRef      map[chat.Ref]*Message // the index of messages, which find reads
	ephemerals []Ephemeral
	members    map[string][]string // channel to users
	users      map[string]chat.User
	bots       map[string]bool
	failures   map[string][]error // method to the errors the next calls return
	sink       chat.Sink
	ready      chan struct{}
	runErr     error
	sent       []Shown // what was received since Sent was last called
}

// NewFake returns a fake with every capability, and limits like a real platform's.
func NewFake() *FakePlatform {
	return &FakePlatform{
		MaxQuestions: 20, Now: time.Now,
		caps: chat.Caps{
			Streams: true, Ephemeral: true, Edits: true, Inputs: true, MaxText: 4000,
			Limits: map[chat.Where]int{chat.WhereHeader: 150, chat.WhereButton: 75, chat.WhereChoice: 75, chat.WhereHint: 150, chat.WhereBody: 12000, chat.WhereStream: 12000},
		},
		byRef: map[chat.Ref]*Message{}, members: map[string][]string{}, users: map[string]chat.User{}, bots: map[string]bool{}, failures: map[string][]error{}, ready: make(chan struct{}),
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

// Messages returns what is posted, oldest first, as copies.
func (f *FakePlatform) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.messages))
	for i, m := range f.messages {
		out[i] = *m
		out[i].Reactions = slices.Clone(m.Reactions)
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

// Say delivers a message from a person to the sink, once Run has started, and returns the sink's error.
func (f *FakePlatform) Say(ctx context.Context, m chat.Inbound) error {
	sink, err := f.running(ctx)
	if err != nil {
		return err
	}
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

// IsMember implements chat.Platform.
func (f *FakePlatform) IsMember(_ context.Context, channel, user string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("IsMember"); err != nil {
		return false, err
	}
	return slices.Contains(f.members[channel], user), nil
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

// Fits implements chat.Platform: a prompt of more than MaxQuestions questions is too large.
func (f *FakePlatform) Fits(p chat.Prompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fits(p)
}

func (f *FakePlatform) fits(p chat.Prompt) error {
	if len(p.Questions) > f.MaxQuestions {
		return fmt.Errorf("%w: %d questions, at most %d", chat.ErrTooLarge, len(p.Questions), f.MaxQuestions)
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
// nothing, so Sent reports no construct.
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
		Inert: func() error { return nil },
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
