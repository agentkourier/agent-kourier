package broker

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// stepClock is a clock a test moves by hand. It starts from a real reading, so its times carry the monotonic clock as
// time.Now's do (Add moves both readings), and time.Time.Sub of two of them is the monotonic difference.
type stepClock struct {
	base time.Time
	off  atomic.Int64
}

func newStepClock() *stepClock { return &stepClock{base: time.Now()} }

func (c *stepClock) now() time.Time          { return c.base.Add(time.Duration(c.off.Load())) }
func (c *stepClock) advance(d time.Duration) { c.off.Add(int64(d)) }

func TestLeaseClockSaysHowLongAgoTheLastAcceptedRenewalBegan(t *testing.T) {
	clock := newStepClock()
	lc := newLeaseClock(clock.now)
	if _, ok := lc.since(); ok {
		t.Fatal("a lease clock that was never told of a renewal knows how long ago it was")
	}
	lc.renewed(clock.now())
	clock.advance(7 * time.Second)
	if got, ok := lc.since(); !ok || got != 7*time.Second {
		t.Errorf("since = %s, %v; want 7s", got, ok)
	}
	// A renewal that began earlier than the one known (the calls of one elector are in order, but a late report must
	// not make the lease look older) changes nothing; one that began later moves it forward.
	lc.renewed(clock.now())
	clock.advance(2 * time.Second)
	lc.renewed(clock.now().Add(-5 * time.Second))
	if got, _ := lc.since(); got != 2*time.Second {
		t.Errorf("since after an older renewal = %s; want 2s, unchanged", got)
	}
	lc.renewed(clock.now())
	if got, _ := lc.since(); got != 0 {
		t.Errorf("since after a newer renewal = %s; want 0", got)
	}
}

// The default clock is time.Now, whose readings carry the monotonic clock: a pause of the whole process (SIGSTOP, a long
// GC pause, a frozen node) is time that passed, on that clock, and the wall clock may say otherwise.
func TestLeaseClockReadsTheMonotonicClockByDefault(t *testing.T) {
	if got := newLeaseClock(nil).now().String(); !strings.Contains(got, "m=") {
		t.Errorf("the default clock's reading = %q; want one with a monotonic part (m=+...)", got)
	}
}

// fakeLock is a resourcelock.Interface that records what it was asked and fails when told to. Every call that it makes
// runs onCall first, which a test uses to let time pass inside the call.
type fakeLock struct {
	resourcelock.Interface
	mu     sync.Mutex
	calls  []string
	fail   error
	onCall func()
}

func (l *fakeLock) record(call string) error {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	hook, fail := l.onCall, l.fail
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
	return fail
}

func (l *fakeLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	return &resourcelock.LeaderElectionRecord{}, nil, l.record("get")
}

func (l *fakeLock) Create(context.Context, resourcelock.LeaderElectionRecord) error {
	return l.record("create")
}

func (l *fakeLock) Update(context.Context, resourcelock.LeaderElectionRecord) error {
	return l.record("update")
}
func (l *fakeLock) Identity() string { return "pod-a_1" }

// The lease clock is told of a renewal only when the API server accepted the write of the Lease: a Create or an Update
// that returned nil. A Get, which extends nothing, and a write that failed (a conflict, an unreachable API server, a lease
// someone else holds) are not one. The time it records is the one at which the call began, so that a call that hung and
// then succeeded does not look fresher than it was.
func TestRenewWatchRecordsOnlyAcceptedWritesAndWhenTheyBegan(t *testing.T) {
	clock := newStepClock()
	lc := newLeaseClock(clock.now)
	lock := &fakeLock{}
	w := renewWatch{Interface: lock, clock: lc}
	ctx := context.Background()

	if _, _, err := w.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := lc.since(); ok {
		t.Error("a Get counted as a renewal")
	}
	lock.fail = errors.New("the API server is unreachable")
	for name, call := range map[string]func() error{
		"create": func() error { return w.Create(ctx, resourcelock.LeaderElectionRecord{}) },
		"update": func() error { return w.Update(ctx, resourcelock.LeaderElectionRecord{}) },
	} {
		if err := call(); !errors.Is(err, lock.fail) {
			t.Errorf("a failed %s returned %v; want the lock's error unchanged", name, err)
		}
	}
	if _, ok := lc.since(); ok {
		t.Error("a write that failed counted as a renewal")
	}

	lock.fail = nil
	lock.onCall = func() { clock.advance(6 * time.Second) } // the call hangs for six seconds, then succeeds
	if err := w.Update(ctx, resourcelock.LeaderElectionRecord{}); err != nil {
		t.Fatal(err)
	}
	if got, ok := lc.since(); !ok || got != 6*time.Second {
		t.Errorf("since after an Update that took 6s = %s, %v; want 6s: the renewal began before the call, not when it returned", got, ok)
	}
	lock.onCall = nil
	clock.advance(time.Second)
	if err := w.Create(ctx, resourcelock.LeaderElectionRecord{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := lc.since(); got != 0 {
		t.Errorf("since after a Create = %s; want 0: the first acquisition is a renewal too", got)
	}
	if w.Identity() != "pod-a_1" {
		t.Error("the watch does not pass the lock's identity on")
	}
}

// recordingPlatform is a chat.Platform that notes each call of it, and answers with zero values.
type recordingPlatform struct {
	mu    sync.Mutex
	calls []string
}

func (p *recordingPlatform) note(name string) {
	p.mu.Lock()
	p.calls = append(p.calls, name)
	p.mu.Unlock()
}

func (p *recordingPlatform) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *recordingPlatform) Run(context.Context, chat.Sink) error { p.note("Run"); return nil }

func (p *recordingPlatform) Post(context.Context, chat.Target, chat.Outgoing, string) (chat.Ref, error) {
	p.note("Post")
	return chat.Ref{}, nil
}

func (p *recordingPlatform) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	p.note("Find")
	return chat.Ref{}, false, nil
}
func (p *recordingPlatform) Update(context.Context, chat.Ref, chat.Outgoing) error {
	p.note("Update")
	return nil
}
func (p *recordingPlatform) RefAt(string, string, string) chat.Ref {
	p.note("RefAt")
	return chat.Ref{}
}

func (p *recordingPlatform) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	p.note("Stream")
	return &recordingStream{p: p}, nil
}
func (p *recordingPlatform) StreamAt(chat.Ref) chat.Stream {
	p.note("StreamAt")
	return &recordingStream{p: p}
}
func (p *recordingPlatform) Ephemeral(context.Context, chat.Target, string, chat.Message) error {
	p.note("Ephemeral")
	return nil
}
func (p *recordingPlatform) React(context.Context, chat.Ref, string) error {
	p.note("React")
	return nil
}
func (p *recordingPlatform) Unreact(context.Context, chat.Ref, string) error {
	p.note("Unreact")
	return nil
}
func (p *recordingPlatform) IsMember(context.Context, string, string) (bool, error) {
	p.note("IsMember")
	return false, nil
}

func (p *recordingPlatform) IsInAnyGroup(context.Context, []string, string) (bool, error) {
	p.note("IsInAnyGroup")
	return false, nil
}

func (p *recordingPlatform) IsBot(context.Context, string) (bool, error) {
	p.note("IsBot")
	return false, nil
}

func (p *recordingPlatform) ResolveUser(context.Context, string) (chat.User, error) {
	p.note("ResolveUser")
	return chat.User{}, nil
}
func (p *recordingPlatform) Fits(chat.Prompt) error { p.note("Fits"); return nil }
func (p *recordingPlatform) Classify(chat.Op, error) chat.Verdict {
	p.note("Classify")
	return chat.Verdict{}
}
func (p *recordingPlatform) Capabilities() chat.Caps { p.note("Capabilities"); return chat.Caps{} }

type recordingStream struct{ p *recordingPlatform }

func (s *recordingStream) Ref() chat.Ref { s.p.note("Stream.Ref"); return chat.Ref{} }
func (s *recordingStream) Append(context.Context, string) error {
	s.p.note("Stream.Append")
	return nil
}
func (s *recordingStream) Step(context.Context, chat.Step) error { s.p.note("Stream.Step"); return nil }
func (s *recordingStream) Stop(context.Context, string, chat.Message, string) error {
	s.p.note("Stream.Stop")
	return nil
}

// What each method of chat.Platform and of chat.Stream does to the chat: a write reaches the platform's conversations and is
// refused once the leader's lease is past its renew deadline; a read passes. A method that is in neither list fails
// TestEveryChatMethodIsClassified, so that a method added to the interface is decided on before it can be called unfenced.
var (
	chatPlatformWrites = map[string]bool{"Post": true, "Update": true, "Stream": true, "Ephemeral": true, "React": true, "Unreact": true}
	chatPlatformReads  = map[string]bool{"Run": true, "Find": true, "RefAt": true, "StreamAt": true, "IsMember": true, "IsInAnyGroup": true, "IsBot": true,
		"ResolveUser": true, "Fits": true, "Classify": true, "Capabilities": true}
	chatStreamWrites = map[string]bool{"Append": true, "Step": true, "Stop": true}
	chatStreamReads  = map[string]bool{"Ref": true}
)

func TestEveryChatMethodIsClassified(t *testing.T) {
	for _, tc := range []struct {
		typ           reflect.Type
		writes, reads map[string]bool
	}{
		{reflect.TypeFor[chat.Platform](), chatPlatformWrites, chatPlatformReads},
		{reflect.TypeFor[chat.Stream](), chatStreamWrites, chatStreamReads},
	} {
		listed := map[string]bool{}
		for i := range tc.typ.NumMethod() {
			name := tc.typ.Method(i).Name
			listed[name] = true
			if tc.writes[name] == tc.reads[name] {
				t.Errorf("%s.%s is %s: a method is a write (fenced) or a read (not), and is in exactly one list; decide in fence_test.go", tc.typ, name,
					map[bool]string{true: "in both lists", false: "in neither list"}[tc.writes[name]])
			}
		}
		for _, list := range []map[string]bool{tc.writes, tc.reads} {
			for name := range list {
				if !listed[name] {
					t.Errorf("%s has no method %s, which a list names", tc.typ, name)
				}
			}
		}
	}
}

// call calls method name of v with a context and zero values, and returns its error if the method has one.
func call(t *testing.T, v any, name string) error {
	t.Helper()
	m := reflect.ValueOf(v).MethodByName(name)
	if !m.IsValid() {
		t.Fatalf("%T has no method %s", v, name)
	}
	ctxType := reflect.TypeFor[context.Context]()
	args := make([]reflect.Value, m.Type().NumIn())
	for i := range args {
		if in := m.Type().In(i); in == ctxType {
			args[i] = reflect.ValueOf(context.Background())
		} else {
			args[i] = reflect.Zero(in)
		}
	}
	out := m.Call(args)
	if n := len(out); n > 0 {
		if err, ok := out[n-1].Interface().(error); ok {
			return err
		}
	}
	return nil
}

// fenceUnderTest is a chat fence over a clock the test moves, with the platform it guards and the reasons it was stopped for.
type fenceUnderTest struct {
	clock    *stepClock
	lease    *leaseClock
	inner    *recordingPlatform
	platform chat.Platform
	mu       sync.Mutex
	stops    []string
}

func newFenceUnderTest(limit time.Duration) *fenceUnderTest {
	f := &fenceUnderTest{clock: newStepClock(), inner: &recordingPlatform{}}
	f.lease = newLeaseClock(f.clock.now)
	f.lease.renewed(f.clock.now())
	f.platform = fenceChat(f.inner, &chatFence{clock: f.lease, limit: limit, stop: func(reason string) {
		f.mu.Lock()
		f.stops = append(f.stops, reason)
		f.mu.Unlock()
	}})
	return f
}

func (f *fenceUnderTest) stopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stops...)
}

// A write passes while the lease was renewed within the renew deadline, the deadline itself included, and from the
// first instant after it is refused: the call is never made, the error says why, and the process is stopped. A read passes
// always. Every method of the platform is tried, found by reflection, so that a method that is added is tried too.
func TestChatFenceRefusesEveryWriteOncePastTheRenewDeadlineAndNoRead(t *testing.T) {
	const limit = 10 * time.Second
	for _, tc := range []struct {
		typ    reflect.Type
		writes map[string]bool
		reads  map[string]bool
		target func(f *fenceUnderTest) any
		prefix string
	}{
		{reflect.TypeFor[chat.Platform](), chatPlatformWrites, chatPlatformReads, func(f *fenceUnderTest) any { return f.platform }, ""},
		{reflect.TypeFor[chat.Stream](), chatStreamWrites, chatStreamReads, func(f *fenceUnderTest) any { s, _ := f.platform.Stream(t.Context(), chat.Target{}, "", ""); return s }, "Stream."},
		{reflect.TypeFor[chat.Stream](), chatStreamWrites, chatStreamReads, func(f *fenceUnderTest) any { return f.platform.StreamAt(chat.Ref{}) }, "Stream."},
	} {
		for i := range tc.typ.NumMethod() {
			name := tc.typ.Method(i).Name
			write := tc.writes[name]
			t.Run(tc.prefix+name+map[bool]string{true: " (a write)", false: " (a read)"}[write], func(t *testing.T) {
				f := newFenceUnderTest(limit)
				target := tc.target(f)
				base := len(f.inner.seen())

				f.clock.advance(limit) // exactly at the deadline: the lease is still the leader's
				if err := call(t, target, name); err != nil {
					t.Fatalf("%s at the renew deadline = %v; want it to pass", name, err)
				}
				if n := len(f.inner.seen()); n != base+1 {
					t.Fatalf("%s at the deadline made %d calls of the platform; want 1", name, n-base)
				}
				if len(f.stopped()) != 0 {
					t.Fatalf("stopped at the deadline: %v", f.stopped())
				}

				f.clock.advance(time.Nanosecond)
				err := call(t, target, name)
				calls := len(f.inner.seen()) - base - 1
				if !write {
					if err != nil || calls != 1 || len(f.stopped()) != 0 {
						t.Errorf("a read past the deadline: err %v, %d calls of the platform, stopped %v; want it to pass, once, and stop nothing", err, calls, f.stopped())
					}
					return
				}
				if !errors.Is(err, ErrChatFenced) {
					t.Errorf("%s past the deadline = %v; want ErrChatFenced", name, err)
				}
				if calls != 0 {
					t.Errorf("%s past the deadline reached the platform %d times; a refused write never does", name, calls)
				}
				if stops := f.stopped(); len(stops) != 1 || !strings.Contains(stops[0], name) || strings.Contains(stops[0], leaseLostReason) {
					t.Errorf("fail-stops = %q; want one that names the refused call (%s) and is not the loss of the lease", stops, name)
				}
			})
		}
	}
}

// A lease clock that was never told of a renewal refuses too: nothing says the leader holds the lease.
func TestChatFenceRefusesWhenTheLeaseWasNeverRenewed(t *testing.T) {
	inner := &recordingPlatform{}
	var stops int
	p := fenceChat(inner, &chatFence{clock: newLeaseClock(nil), limit: time.Hour, stop: func(string) { stops++ }})
	if _, err := p.Post(t.Context(), chat.Target{}, chat.Outgoing{}, "tag"); !errors.Is(err, ErrChatFenced) || len(inner.seen()) != 0 || stops != 1 {
		t.Errorf("Post = %v, %d platform calls, %d stops; want ErrChatFenced, none, one", err, len(inner.seen()), stops)
	}
}

// A stream that a lease still good began is fenced when the lease is not: its appends and its stop are the writes that
// a leader frozen mid-reply would otherwise finish (S16).
func TestChatFenceRefusesTheAppendsOfAStreamThatWasOpenWhenTheLeaseLapsed(t *testing.T) {
	f := newFenceUnderTest(10 * time.Second)
	s, err := f.platform.Stream(t.Context(), chat.Target{Channel: "C1"}, "U1", "the first chunk")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(t.Context(), " and the second"); err != nil {
		t.Fatalf("an append while the lease is good = %v", err)
	}
	f.clock.advance(time.Minute) // the process was frozen
	if err := s.Append(t.Context(), " and the third"); !errors.Is(err, ErrChatFenced) {
		t.Errorf("an append after the freeze = %v; want ErrChatFenced", err)
	}
	if err := s.Stop(t.Context(), "", chat.Message{}, "tag"); !errors.Is(err, ErrChatFenced) {
		t.Errorf("the stop after the freeze = %v; want ErrChatFenced", err)
	}
	if got := f.inner.seen(); !reflect.DeepEqual(got, []string{"Stream", "Stream.Append"}) {
		t.Errorf("the platform saw %v; want the start and the one append that the lease allowed", got)
	}
}

// The wrapper adds nothing to what the Slack adapter says of itself: a link is still built where it could be.
type permalinkPlatform struct{ *recordingPlatform }

func (permalinkPlatform) Permalink(r chat.Ref) (string, bool) { return "link/" + r.ID, true }

func TestChatFencePassesPermalinksOn(t *testing.T) {
	f := &chatFence{clock: newLeaseClock(nil), limit: time.Hour, stop: func(string) {}}
	with, ok := fenceChat(permalinkPlatform{&recordingPlatform{}}, f).(chat.Permalinker)
	if !ok {
		t.Fatal("the fenced platform is not a chat.Permalinker")
	}
	if got, ok := with.Permalink(chat.Ref{ID: "1.2"}); !ok || got != "link/1.2" {
		t.Errorf("Permalink = %q, %v; want the platform's, with no lease needed: it is a read", got, ok)
	}
	if got, ok := permalinks(fenceChat(&recordingPlatform{}, f))(chat.Ref{ID: "1.2"}); ok || got != "" {
		t.Errorf("Permalink of a platform that makes none = %q, %v; want none", got, ok)
	}
}

// threadPlatform is a platform that reads threads, and notes what it was asked.
type threadPlatform struct {
	*recordingPlatform
	asked []string
}

func (p *threadPlatform) ThreadBetween(_ context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	p.note("ThreadBetween")
	p.asked = append(p.asked, strings.Join([]string{t.Channel, t.Thread, after, before, strconv.Itoa(limit)}, "|"))
	return []chat.HistoryMessage{{Message: chat.Inbound{Text: "a reply"}}}, 3, errThreadRead
}

var errThreadRead = errors.New("the platform's own error")

// A thread read is a read: it passes the fence whatever the lease says (this fence has never seen a renewal, which stops
// the process for a write), and it gets the platform's answer as it is. A platform that cannot
// read a thread is told of by the sentinel, from the wrapper that every platform is behind.
func TestChatFencePassesThreadReadsOnAndSaysWhenThePlatformCannot(t *testing.T) {
	var stops []string
	fence := &chatFence{clock: newLeaseClock(nil), limit: time.Second, stop: func(reason string) { stops = append(stops, reason) }} // never renewed: a write is refused
	inner := &threadPlatform{recordingPlatform: &recordingPlatform{}}
	reader, ok := fenceChat(inner, fence).(chat.ThreadReader)
	if !ok {
		t.Fatal("the fenced platform is not a chat.ThreadReader")
	}
	msgs, dropped, err := reader.ThreadBetween(context.Background(), chat.Target{Channel: "C1", Thread: "1.1"}, "1.2", "1.9", 7)
	if len(msgs) != 1 || msgs[0].Message.Text != "a reply" || dropped != 3 || !errors.Is(err, errThreadRead) {
		t.Errorf("ThreadBetween = %+v, %d, %v; want the platform's answer unchanged", msgs, dropped, err)
	}
	if want := []string{"C1|1.1|1.2|1.9|7"}; !slices.Equal(inner.asked, want) {
		t.Errorf("the platform was asked %v; want %v", inner.asked, want)
	}

	bare := &recordingPlatform{}
	reader = fenceChat(bare, fence).(chat.ThreadReader)
	if msgs, dropped, err := reader.ThreadBetween(context.Background(), chat.Target{}, "", "", 1); !errors.Is(err, chat.ErrNoThreadReads) || msgs != nil || dropped != 0 {
		t.Errorf("ThreadBetween of a platform that cannot = %+v, %d, %v; want ErrNoThreadReads", msgs, dropped, err)
	}
	if n := len(bare.seen()); n != 0 {
		t.Errorf("the platform that cannot was called %d times: %v", n, bare.seen())
	}
	if len(stops) != 0 {
		t.Errorf("a read stopped the process: %v", stops)
	}
	if _, err := fenceChat(inner, fence).Post(context.Background(), chat.Target{}, chat.Outgoing{}, ""); !errors.Is(err, ErrChatFenced) {
		t.Errorf("a write through the same fence = %v; want it refused, so that the read's passing says something", err)
	}
}

// The bound is the renew deadline, the time after which the elector gives up and the process ends, and not the lease
// duration, which is when a candidate may take the Lease: the old leader must have stopped before any candidate may start,
// with a margin for the call in flight and the clocks of two machines (the 5 s of the defaults).
func TestChatFenceIsBoundedByTheRenewDeadlineNotTheLeaseDuration(t *testing.T) {
	for name, cfg := range map[string]ElectionConfig{
		"the defaults":                      {},
		"the timings of the chart's values": {LeaseDuration: 9 * time.Second, RenewDeadline: 6 * time.Second, RetryPeriod: 2 * time.Second},
		"a test's":                          testElection,
	} {
		f := newChatFence(cfg, newLeaseClock(nil), func(string) {})
		want := cfg.withDefaults()
		if f.limit != want.RenewDeadline || f.limit >= want.LeaseDuration {
			t.Errorf("%s: the fence's limit is %s; want the renew deadline, %s, which is under the lease duration, %s", name, f.limit, want.RenewDeadline, want.LeaseDuration)
		}
	}
}
