package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// ErrChatFenced is the error of a chat write that was refused because the leader's lease is past its renew deadline: this
// replica can no longer say that it leads, and another may. The refusal also stops the process (failStop, exit 5), so the
// error is for the one call that is refused, and for a test.
var ErrChatFenced = errors.New("broker: chat write refused: the leader's lease was not renewed within its renew deadline")

// leaseClock keeps when this replica last renewed its Lease, on the monotonic clock. A leader that is paused as a whole (a
// SIGSTOP, a frozen node, a long garbage-collection pause) renews nothing while the pause lasts, and the monotonic clock goes
// on: the first call after it finds the lease old, which the Lease itself cannot tell it, since only its own renewals touch it.
type leaseClock struct {
	now  func() time.Time // time.Now, whose readings carry the monotonic clock; a test's own
	mu   sync.Mutex
	last time.Time // when the newest accepted renewal began; zero before the first
}

func newLeaseClock(now func() time.Time) *leaseClock {
	if now == nil {
		now = time.Now
	}
	return &leaseClock{now: now}
}

// renewed records an accepted renewal that began at began. An earlier one than is known changes nothing.
func (c *leaseClock) renewed(began time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if began.After(c.last) {
		c.last = began
	}
}

// since is how long ago the newest accepted renewal began, and false if none was accepted yet. It is time.Time.Sub of two
// readings of the clock, which is the monotonic difference when both have one.
func (c *leaseClock) since() (time.Duration, bool) {
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	if last.IsZero() {
		return 0, false
	}
	return c.now().Sub(last), true
}

// renewWatch is the resource lock that client-go's elector writes the Lease through, with one addition: it tells the lease
// clock of each write the API server accepted. That is the one place a renewal is observable. The elector offers no callback
// for it (OnNewLeader is for a change of holder, and only the elector's own goroutine knows when it last tried), and a renewal
// is an Update of the Lease (a Create or an Update, for the first acquisition), which succeeds only if the Lease is still as
// this replica last wrote it: the API server's resource version makes a write after another replica's a conflict. So a write
// that returned nil means this replica held the Lease at that moment, and nothing else does: a Get extends nothing, and a
// failed write is no renewal.
//
// The time recorded is the one at which the write began, not when it returned: a candidate takes the Lease only after it has
// seen it unchanged for a lease duration, which starts no earlier than the write was applied, so the begin of the call is the
// earliest that can be, and a call that hung and then succeeded looks no fresher than it began.
type renewWatch struct {
	resourcelock.Interface
	clock *leaseClock
}

func (w renewWatch) Create(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	began := w.clock.now()
	if err := w.Interface.Create(ctx, ler); err != nil {
		return err
	}
	w.clock.renewed(began)
	return nil
}

func (w renewWatch) Update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	began := w.clock.now()
	if err := w.Interface.Update(ctx, ler); err != nil {
		return err
	}
	w.clock.renewed(began)
	return nil
}

// chatFence refuses a leader's writes to the chat once its lease is past the renew deadline, whatever the store says: the
// store's epoch fence (store.ErrDeposed) stops a deposed leader's writes to the database at its first one, but a leader that
// was paused as a whole can have a whole stream of Slack calls to make before it writes to the database, and nothing else
// would stop them (spec D37). The limit is the renew deadline and not the lease duration because it is the elector's own
// bound: a leader that has not renewed for that long gives up and exits, so the old leader stops before the lease duration
// passes, and the margin between the two (5 s by default) is what a candidate waits besides, for the request in flight, for
// the clocks of the two not being one. At the lease duration there would be no margin at all.
type chatFence struct {
	clock *leaseClock
	limit time.Duration // the renew deadline
	stop  func(reason string)
}

// newChatFence is the fence of a leader that holds the Lease of cfg, whose lease clock is lease: bounded by the renew
// deadline of the election, which is less than the lease duration by the margin the timings leave (validate).
func newChatFence(cfg ElectionConfig, lease *leaseClock, stop func(reason string)) *chatFence {
	return &chatFence{clock: lease, limit: cfg.withDefaults().RenewDeadline, stop: stop}
}

// allow says whether the chat write op may be made now. If it may not, the process is stopped (stop, with the reason) and
// the error is ErrChatFenced.
func (f *chatFence) allow(op string) error {
	age, renewed := f.clock.since()
	if renewed && age <= f.limit {
		return nil
	}
	if renewed {
		f.stop(fmt.Sprintf("a chat write (%s) was refused: the lease was last renewed %s ago, past the renew deadline of %s", op, age.Round(time.Millisecond), f.limit))
	} else {
		f.stop(fmt.Sprintf("a chat write (%s) was refused: the lease has not been renewed since this replica led", op))
	}
	return fmt.Errorf("%w: %s", ErrChatFenced, op)
}

// fenceChat returns p with every write to the chat checked by f first: a post, an edit, a stream (its start, its appends, its
// steps and its stop), an ephemeral and a reaction. A write that f refuses does not reach p. What only reads (the connection,
// a lookup, a user, the platform's limits) is p's own: a replica that cannot lead can still read, and the connection is
// what hears the events that the new leader's own connection also hears.
func fenceChat(p chat.Platform, f *chatFence) chat.Platform {
	return fencedPlatform{Platform: p, fence: f}
}

type fencedPlatform struct {
	chat.Platform
	fence *chatFence
}

func (p fencedPlatform) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	if err := p.fence.allow("Post"); err != nil {
		return chat.Ref{}, err
	}
	return p.Platform.Post(ctx, t, o, tag)
}

func (p fencedPlatform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	if err := p.fence.allow("Update"); err != nil {
		return err
	}
	return p.Platform.Update(ctx, r, o)
}

func (p fencedPlatform) Stream(ctx context.Context, t chat.Target, to, markdown string) (chat.Stream, error) {
	if err := p.fence.allow("Stream"); err != nil {
		return nil, err
	}
	s, err := p.Platform.Stream(ctx, t, to, markdown)
	if err != nil {
		return nil, err
	}
	return fencedStream{Stream: s, fence: p.fence}, nil
}

// StreamAt makes no call, so it is not a write; the stream it returns is fenced, as the one Stream returns.
func (p fencedPlatform) StreamAt(r chat.Ref) chat.Stream {
	return fencedStream{Stream: p.Platform.StreamAt(r), fence: p.fence}
}

func (p fencedPlatform) Ephemeral(ctx context.Context, t chat.Target, user string, m chat.Message) error {
	if err := p.fence.allow("Ephemeral"); err != nil {
		return err
	}
	return p.Platform.Ephemeral(ctx, t, user, m)
}

func (p fencedPlatform) React(ctx context.Context, r chat.Ref, name string) error {
	if err := p.fence.allow("React"); err != nil {
		return err
	}
	return p.Platform.React(ctx, r, name)
}

func (p fencedPlatform) Unreact(ctx context.Context, r chat.Ref, name string) error {
	if err := p.fence.allow("Unreact"); err != nil {
		return err
	}
	return p.Platform.Unreact(ctx, r, name)
}

// Permalink is chat.Permalinker, which the platform may be: a link is built from what the platform learned, with no call,
// so it is a read.
func (p fencedPlatform) Permalink(r chat.Ref) (string, bool) {
	if l, ok := p.Platform.(chat.Permalinker); ok {
		return l.Permalink(r)
	}
	return "", false
}

// ThreadBetween is chat.ThreadReader, which the platform may be: it reads a thread and changes nothing, so it is no write
// and the fence does not check it. The method exists so that the fence does not hide the capability of the platform it
// wraps, as it does not hide Permalink; a platform that cannot read a thread is chat.ErrNoThreadReads. The fence is only
// there when the election is (with SQLite there is none), and the built platform hides the method too, so a caller asks the
// built platform's inner Platform with a type assertion, as the trigger start does for chat.Permalinker.
func (p fencedPlatform) ThreadBetween(ctx context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	if r, ok := p.Platform.(chat.ThreadReader); ok {
		return r.ThreadBetween(ctx, t, after, before, limit)
	}
	return nil, 0, chat.ErrNoThreadReads
}

type fencedStream struct {
	chat.Stream
	fence *chatFence
}

func (s fencedStream) Append(ctx context.Context, markdown string) error {
	if err := s.fence.allow("Stream.Append"); err != nil {
		return err
	}
	return s.Stream.Append(ctx, markdown)
}

func (s fencedStream) Step(ctx context.Context, st chat.Step) error {
	if err := s.fence.allow("Stream.Step"); err != nil {
		return err
	}
	return s.Stream.Step(ctx, st)
}

func (s fencedStream) Stop(ctx context.Context, markdown string, final chat.Message, tag string) error {
	if err := s.fence.allow("Stream.Stop"); err != nil {
		return err
	}
	return s.Stream.Stop(ctx, markdown, final, tag)
}
