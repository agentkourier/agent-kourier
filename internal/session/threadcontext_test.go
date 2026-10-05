package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// windowCall is one call of Config.ThreadContext.
type windowCall struct {
	key                    Key
	binding, after, before string
}

// threadHook is a Config.ThreadContext that records its calls and answers with section.
type threadHook struct {
	mu      sync.Mutex
	section string
	calls   []windowCall
}

func (h *threadHook) fn(_ context.Context, k Key, binding, after, before string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, windowCall{k, binding, after, before})
	return h.section
}

func (h *threadHook) seen() []windowCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

const threadSection = "Replies in the thread since your last turn: <block>"

// previousRef is the message of the thread's earlier turn, and queuedRef that of the turn under test.
const previousRef, queuedRef = "1700.10", "1700.50"

// threadedFixture is a conversation in a Binding of the given mode whose earlier turn was sent: the session
// exists, and the queue holds that turn's tombstone. A runner is live, the hook is wired, and nothing is queued.
func threadedFixture(t *testing.T, mode v1alpha1.ThreadReplies) (*fixture, *fakeSink, *threadHook) {
	return thread(t, mode, true)
}

// thread is threadedFixture, with or without the earlier turn: without it, the session has no row in the queue.
func thread(t *testing.T, mode v1alpha1.ThreadReplies, previous bool) (*fixture, *fakeSink, *threadHook) {
	t.Helper()
	f := newFixture(t)
	sink := f.live()
	f.threadReplies = mode
	hook := &threadHook{section: threadSection}
	f.m.cfg.ThreadContext = hook.fn
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, ContextID: "c1", Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	if previous {
		f.enqueue(t, "prev", previousRef, "U1", true)
	}
	return f, sink, hook
}

// enqueue puts a turn in the queue directly, and marks it delivered when sent says so. It does not wake the runner.
func (f *fixture) enqueue(t *testing.T, id, ref, user string, sent bool) {
	t.Helper()
	r := store.Reply{ID: id, Key: key, UserID: user, MessageRef: ref, Text: "hello", EnqueuedAt: t0}
	if ok, err := f.st.EnqueueReply(t.Context(), r); err != nil || !ok {
		t.Fatalf("enqueue %s = %v, %v", id, ok, err)
	}
	if sent {
		if err := f.st.MarkReplyDelivered(t.Context(), id, t0); err != nil {
			t.Fatal(err)
		}
	}
}

// sentText is the text of the n-th message the agent was sent.
func (f *fixture) sentText(t *testing.T, n int) string {
	t.Helper()
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if n >= len(f.agent.msgs) {
		t.Fatalf("the agent was sent %d messages; want a %dth", len(f.agent.msgs), n+1)
	}
	msg := f.agent.msgs[n]
	if len(msg.Parts) != 1 {
		t.Fatalf("message %d has %d parts; want one (R18)", n, len(msg.Parts))
	}
	return msg.Parts[0].Text()
}

// A mentioned turn in a mention-only Binding carries the section the hook returns, after the person's text, and
// the hook is asked about the window from the previous turn's message to this one's. The first turn of a thread
// has no previous turn, and so no section and no call.
func TestThreadContextIsAppendedToAMentionedTurn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.threadReplies = v1alpha1.ThreadRepliesMention
	hook := &threadHook{section: threadSection}
	f.m.cfg.ThreadContext = hook.fn

	first := inbound("1700.10")
	first.Mentioned = true
	if _, err := f.submit(first); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if got := f.sentText(t, 0); got != "hello" || len(hook.seen()) != 0 {
		t.Errorf("first turn = %q, hook calls %v; want the person's text alone and no call", got, hook.seen())
	}

	second := inbound("1700.50")
	second.Create, second.Mentioned = false, true
	if _, err := f.submit(second); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if got, want := f.sentText(t, 1), "hello\n\n"+threadSection; got != want {
		t.Errorf("second turn = %q; want the person's text, a blank line and the section: %q", got, want)
	}
	want := []windowCall{{key, "ns/b1", "1700.10", "1700.50"}}
	if got := hook.seen(); !slices.Equal(got, want) {
		t.Errorf("hook calls = %+v; want %+v: the window is the previous turn's message to this one's", got, want)
	}
}

// The hook is asked only for a mention-only Binding's turn that a person wrote, that is not pinned to a question,
// and that has a turn before it; anything else is sent as it was queued, and the hook is never called.
func TestThreadContextIsAskedOnlyWhereItBelongs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     v1alpha1.ThreadReplies
		user     string
		pinned   bool
		previous bool
		unset    bool // no hook is configured
		want     bool // the hook is called, and its section appended
	}{
		{name: "a mention-only turn", mode: v1alpha1.ThreadRepliesMention, user: "U1", previous: true, want: true},
		{name: "all replies", mode: v1alpha1.ThreadRepliesAll, user: "U1", previous: true},
		{name: "the mode is empty", mode: "", user: "U1", previous: true},
		{name: "a turn nobody wrote", mode: v1alpha1.ThreadRepliesMention, user: SystemUser, previous: true},
		{name: "a reply pinned to a question", mode: v1alpha1.ThreadRepliesMention, user: "U1", pinned: true, previous: true},
		{name: "the first turn of the thread", mode: v1alpha1.ThreadRepliesMention, user: "U1"},
		{name: "no hook configured", mode: v1alpha1.ThreadRepliesMention, user: "U1", previous: true, unset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, hook := thread(t, tc.mode, tc.previous)
			if tc.unset {
				f.m.cfg.ThreadContext = nil
			}
			f.enqueue(t, "turn", queuedRef, tc.user, false)
			if tc.pinned {
				claim, ok := f.st.Store.(interface {
					ClaimReplyInteraction(ctx context.Context, id, interaction string) error
				})
				if !ok {
					t.Fatal("the store cannot pin a reply")
				}
				if err := claim.ClaimReplyInteraction(t.Context(), "turn", "q1"); err != nil {
					t.Fatal(err)
				}
			}
			f.m.wake(key)
			waitFinal(t, sink)
			want, calls := "hello", 0
			if tc.want {
				want, calls = "hello\n\n"+threadSection, 1
			}
			if got := f.sentText(t, 0); got != want || len(hook.seen()) != calls {
				t.Errorf("sent %q after %d hook calls; want %q after %d", got, len(hook.seen()), want, calls)
			}
		})
	}
}

// The section comes after the person's text, so the attribution the dialect puts in front of the one text part
// names only the person who mentioned courier.
func TestThreadContextComesAfterTheAttribution(t *testing.T) {
	f, sink, _ := threadedFixture(t, v1alpha1.ThreadRepliesMention)
	f.attr = (&kagentv1.Dialect{}).Attribute
	f.enqueue(t, "turn", queuedRef, "U2", false)
	f.m.wake(key)
	waitFinal(t, sink)
	if got, want := f.sentText(t, 0), "[Slack: U2]\nhello\n\n"+threadSection; got != want {
		t.Errorf("sent %q; want the attribution, the person's text, then the section: %q", got, want)
	}
}

// A hook that has nothing to add returns "", and the turn goes as it was queued.
func TestThreadContextOfNothingAddsNothing(t *testing.T) {
	f, sink, hook := threadedFixture(t, v1alpha1.ThreadRepliesMention)
	hook.section = ""
	f.enqueue(t, "turn", queuedRef, "U1", false)
	f.m.wake(key)
	waitFinal(t, sink)
	if got := f.sentText(t, 0); got != "hello" || len(hook.seen()) != 1 {
		t.Errorf("sent %q after %d hook calls; want the person's text alone, the hook having been asked", got, len(hook.seen()))
	}
}

// A turn sent again after -32004 is the same turn: the hook ran once, for its first send, and every send carries
// that section. A hook that reads the thread is not asked again for each retry.
func TestThreadContextIsAskedOnceAcrossBusyRetries(t *testing.T) {
	f, sink, hook := threadedFixture(t, v1alpha1.ThreadRepliesMention)
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n < 3 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	f.enqueue(t, "turn", queuedRef, "U1", false)
	f.m.wake(key)
	waitFinal(t, sink)
	if sends, _, _ := f.agent.counts(); sends != 4 {
		t.Fatalf("%d sends; want the three rejected and the one taken", sends)
	}
	for n := range 4 {
		if got, want := f.sentText(t, n), "hello\n\n"+threadSection; got != want {
			t.Errorf("send %d = %q; want %q", n, got, want)
		}
	}
	if got := hook.seen(); len(got) != 1 {
		t.Errorf("hook calls = %+v; want one for the four sends", got)
	}
}

// The cache is of the turn that was sent: the next turn of the thread is asked for its own window, from the row
// before it, and does not get the section of the turn ahead of it.
func TestThreadContextCacheIsPerTurn(t *testing.T) {
	f, sink, hook := threadedFixture(t, v1alpha1.ThreadRepliesMention)
	f.enqueue(t, "turn1", "1700.50", "U1", false)
	f.enqueue(t, "turn2", "1700.70", "U1", false)
	f.m.wake(key)
	waitFinal(t, sink)
	waitFinal(t, sink)
	want := []windowCall{{key, "ns/b1", previousRef, "1700.50"}, {key, "ns/b1", "1700.50", "1700.70"}}
	if got := hook.seen(); !slices.Equal(got, want) {
		t.Errorf("hook calls = %+v; want %+v: each turn has its own window, from the row before it", got, want)
	}
}

// After a restart, a fresh Manager over the same store asks for the same window, because it comes from stored rows,
// and sends the same text.
func TestThreadContextWindowIsTheSameAfterARestart(t *testing.T) {
	f, sink, hook := threadedFixture(t, v1alpha1.ThreadRepliesMention)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = 20*time.Millisecond, 20*time.Millisecond // few sends before the restart
	f.agent.script = func(context.Context, int, agent.Handler) error { return fmt.Errorf("send message: %w", agent.ErrBusy) }
	f.enqueue(t, "turn", queuedRef, "U1", false)
	f.m.wake(key)
	eventually(t, "the first process to send the turn", func() bool { sends, _, _ := f.agent.counts(); return sends >= 1 })
	if err := f.close(); err != nil { // the process dies
		t.Fatal(err)
	}
	before, first := hook.seen(), f.sentText(t, 0)

	// The restarted process: a new Manager over the same store, a hook of its own, and an agent that takes the turn.
	again := &threadHook{section: threadSection}
	cfg := f.m.cfg
	cfg.ThreadContext = again.fn
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	f.m.wake = f.m.start
	f.agent.mu.Lock()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error { return turn(h, n, a2a.TaskStateCompleted) }
	f.agent.mu.Unlock()
	f.m.wake(key)
	waitFinal(t, sink)

	want := []windowCall{{key, "ns/b1", previousRef, queuedRef}}
	if !slices.Equal(before, want) || !slices.Equal(again.seen(), want) {
		t.Errorf("hook calls before the restart %+v and after %+v; want both %+v", before, again.seen(), want)
	}
	f.agent.mu.Lock()
	last := f.agent.msgs[len(f.agent.msgs)-1].Parts[0].Text()
	f.agent.mu.Unlock()
	if last != first || first != "hello\n\n"+threadSection {
		t.Errorf("text before the restart %q and after %q; want the same, with the section", first, last)
	}
}

// A task that is found on the agent is followed, not sent: the turn was sent before, so the hook is not asked
// again for it. Neither is it asked when recovery follows a recorded task.
func TestThreadContextIsNotAskedWhenATaskIsFollowed(t *testing.T) {
	t.Run("a resend finds its task", func(t *testing.T) {
		f, sink, hook := threadedFixture(t, v1alpha1.ThreadRepliesMention)
		f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
			return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
		}
		f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
		// A turn that this process did not enqueue may have been sent before: it is looked up first.
		f.enqueue(t, "turn", queuedRef, "U1", false)
		f.m.wake(key)
		waitFinal(t, sink)
		if sends, finds, follows := f.agent.counts(); sends != 0 || finds != 1 || follows != 1 {
			t.Fatalf("%d sends, %d lookups, %d follows; want the found task followed and nothing sent", sends, finds, follows)
		}
		if got := hook.seen(); len(got) != 0 {
			t.Errorf("hook calls = %+v; want none for a turn that is followed", got)
		}
	})
	t.Run("recovery follows a recorded task", func(t *testing.T) {
		f, sink := restarted(t)
		f.threadReplies = v1alpha1.ThreadRepliesMention
		hook := &threadHook{section: threadSection}
		f.m.cfg.ThreadContext = hook.fn
		f.enqueue(t, "prev", previousRef, "U1", true) // the turn that started the recorded task
		f.recover(t)
		waitFinal(t, sink)
		if _, _, follows := f.agent.counts(); follows != 1 || len(hook.seen()) != 0 {
			t.Errorf("%d follows, hook calls %+v; want the task followed and the hook not asked", follows, hook.seen())
		}
	})
}

// failingPrevious fails PreviousReply once, with a store error.
type failingPrevious struct {
	Store
	mu    sync.Mutex
	fails int
}

func (s *failingPrevious) PreviousReply(ctx context.Context, k Key, seq int64) (store.Reply, error) {
	s.mu.Lock()
	failed := s.fails > 0
	if failed {
		s.fails--
	}
	s.mu.Unlock()
	if failed {
		return store.Reply{}, errors.New("database is locked")
	}
	return s.Store.PreviousReply(ctx, k, seq)
}

// A store that cannot say which turn came before leaves the window unknown: the turn is not sent without its context
// (it cannot be sent again with it), and is tried again, with the hook asked once the window is known. The thread
// is told nothing: the failures are the store's, and the agent was not called.
func TestThreadContextWaitsForTheStore(t *testing.T) {
	f, sink, hook := threadedFixture(t, v1alpha1.ThreadRepliesMention)
	f.m.cfg.Store = &failingPrevious{Store: f.st, fails: 2}
	f.enqueue(t, "turn", queuedRef, "U1", false)
	f.m.wake(key)
	waitFinal(t, sink)
	if sends, _, _ := f.agent.counts(); sends != 1 {
		t.Errorf("%d sends; want the turn sent once, after the store answered", sends)
	}
	if got, want := f.sentText(t, 0), "hello\n\n"+threadSection; got != want {
		t.Errorf("sent %q; want %q", got, want)
	}
	if got := hook.seen(); len(got) != 1 {
		t.Errorf("hook calls = %+v; want one", got)
	}
	if got := sink.noticesOf(NoticeRetrying); len(got) != 0 {
		t.Errorf("notices = %+v; want none: the agent was never called, so the thread is not told it cannot be reached", got)
	}
}
