package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var (
	t0  = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	key = Key{Connection: "slack", Channel: "C1", ThreadTS: "1700.01"}
)

// spyStore counts touches and fails a named call ("touch", "upsert", "enqueue", "set" for
// SetSessionTask or "mark" for MarkReplyDelivered), once or every time, over a real SQLite store.
type spyStore struct {
	Store
	mu      sync.Mutex
	touches int
	upserts int
	fail    map[string]error // fails a call once
	stuck   map[string]error // fails a call every time
	gate    chan struct{}    // when set, the first GetSession waits for a second, or 200 ms
	reads   int
}

func (s *spyStore) GetSession(ctx context.Context, k Key, ttl time.Duration, now time.Time) (store.Session, error) {
	s.mu.Lock()
	s.reads++
	n := s.reads
	s.mu.Unlock()
	switch {
	case s.gate != nil && n == 1:
		select {
		case <-s.gate:
		case <-time.After(200 * time.Millisecond):
		}
	case s.gate != nil && n == 2:
		close(s.gate)
	}
	return s.Store.GetSession(ctx, k, ttl, now)
}

func (s *spyStore) failOnce(call string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stuck[call]; err != nil {
		return err
	}
	err := s.fail[call]
	delete(s.fail, call)
	return err
}

func (s *spyStore) SetSessionTask(ctx context.Context, k Key, contextID, taskID string) error {
	if err := s.failOnce("set"); err != nil {
		return err
	}
	return s.Store.SetSessionTask(ctx, k, contextID, taskID)
}

func (s *spyStore) MarkReplyDelivered(ctx context.Context, id string, at time.Time) error {
	if err := s.failOnce("mark"); err != nil {
		return err
	}
	return s.Store.MarkReplyDelivered(ctx, id, at)
}

func (s *spyStore) TouchSession(ctx context.Context, k Key, at time.Time) error {
	s.mu.Lock()
	s.touches++
	s.mu.Unlock()
	if err := s.failOnce("touch"); err != nil {
		return err
	}
	return s.Store.TouchSession(ctx, k, at)
}

func (s *spyStore) UpsertSession(ctx context.Context, sess store.Session) error {
	s.mu.Lock()
	s.upserts++
	s.mu.Unlock()
	if err := s.failOnce("upsert"); err != nil {
		return err
	}
	return s.Store.UpsertSession(ctx, sess)
}

func (s *spyStore) EnqueueReply(ctx context.Context, r store.Reply) (bool, error) {
	if err := s.failOnce("enqueue"); err != nil {
		return false, err
	}
	return s.Store.EnqueueReply(ctx, r)
}

type fixture struct {
	t                *testing.T
	st               *spyStore
	m                *Manager
	woke             int
	onWake           func()
	now              atomic.Int64 // the clock, in Unix nanoseconds
	agent            *fakeAgent
	real             AgentAPI // when set, the agent the Binding uses instead of the fake
	amu              sync.Mutex
	rejected, audits []audit.Call // what Rejected and the Auditor were given
	attr             func(*a2a.Message, dialect.User)
}

func (f *fixture) reject(_ context.Context, _ audit.Actor, c audit.Call) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.rejected = append(f.rejected, c)
	return nil
}

// AgentCall makes the fixture the Auditor.
func (f *fixture) AgentCall(_ context.Context, _ audit.Actor, c audit.Call) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.audits = append(f.audits, c)
	return nil
}

// Backend makes the fixture the Bindings: ns/b1 has a threadTTL of 1 h, ns/b2 of 1000 h.
func (f *fixture) Backend(name string) (Backend, error) {
	ttl, ok := map[string]time.Duration{"ns/b1": time.Hour, "ns/b2": 1000 * time.Hour}[name]
	if !ok {
		return Backend{}, ErrUnknownBinding
	}
	var ag AgentAPI = f.agent
	if f.real != nil {
		ag = f.real
	}
	return Backend{ThreadTTL: ttl, Agent: ag, Attribute: f.attr, Rejected: f.reject, Identity: "secret:ns/token"}, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "courier.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, st: &spyStore{Store: s, fail: map[string]error{}, stuck: map[string]error{}}, agent: newFakeAgent()}
	f.now.Store(t0.UnixNano())
	f.m, err = New(Config{Store: f.st, Bindings: f, Audit: f, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return time.Unix(0, f.now.Load()).UTC() }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.m.wake = func(Key) { f.onWake(); f.woke++ }
	f.onWake = func() {}
	return f
}

// inbound is the mention in Slack message ts that may create the session under ns/b1.
func inbound(ts string) Inbound {
	return Inbound{Key: key, Binding: "ns/b1", MessageID: SlackMessageID("slack", "C1", ts), MessageTS: ts, UserID: "U1", Text: "hello", Create: true}
}

func (f *fixture) submit(in Inbound) (Result, error) { return f.m.Submit(f.t.Context(), in) }

func (f *fixture) session() (store.Session, error) {
	return f.st.GetSession(f.t.Context(), key, 100*365*24*time.Hour, t0)
}

func (f *fixture) seed(binding string, age time.Duration) store.Session {
	f.t.Helper()
	sess := store.Session{Key: key, ContextID: "c1", TaskID: "t1", Binding: binding, LastActivity: t0.Add(-age)}
	if err := f.st.UpsertSession(f.t.Context(), sess); err != nil {
		f.t.Fatal(err)
	}
	f.st.touches = 0
	return sess
}

func (f *fixture) drain() (out []store.Reply) {
	for {
		r, err := f.st.NextReply(f.t.Context(), key)
		if err != nil {
			return out
		}
		out = append(out, r)
		_ = f.st.MarkReplyDelivered(f.t.Context(), r.ID, t0)
	}
}

func TestSubmitNotOwned(t *testing.T) {
	f := newFixture(t)
	in := inbound("1700.01")
	in.Create = false
	res, err := f.submit(in)
	if _, serr := f.session(); err != nil || res.Disposition != NotOwned || !errors.Is(serr, store.ErrNotFound) || f.woke != 0 || len(f.drain()) != 0 {
		t.Errorf("Submit = %+v, %v; session %v, woke %d; want NotOwned and nothing written or woken", res, err, serr, f.woke)
	}
}

// The row (key, Binding, no contextId) and the turn are written before the runner wakes
// (spec 8.2: the row precedes the first A2A call).
func TestSubmitCreatesTheSessionBeforeWaking(t *testing.T) {
	f := newFixture(t)
	f.onWake = func() {
		sess, err := f.session()
		if err != nil || sess.Binding != "ns/b1" || sess.ContextID != "" || sess.TaskID != "" || !sess.LastActivity.Equal(t0) {
			t.Errorf("session at wake = %+v, %v", sess, err)
		}
		if r, err := f.st.NextReply(t.Context(), key); err != nil || r.ID != "slack:slack:C1:1700.01" || r.UserID != SystemUser || r.Text != "hello" || r.MessageTS != "1700.01" {
			t.Errorf("queue at wake = %+v, %v", r, err)
		}
	}
	in := inbound("1700.01")
	in.UserID = "" // an alert's prompt, which no Slack user wrote
	if res, err := f.submit(in); err != nil || res.Disposition != Created || f.woke != 1 {
		t.Fatalf("Submit = %+v, %v, woke %d; want Created and one wake", res, err, f.woke)
	}
}

// One Slack message arrives as two events and may be redelivered: it is one queued turn
// (8.2), also after it was sent (D21). Every Submit still wakes the runner.
func TestSubmitCollapsesDuplicates(t *testing.T) {
	f := newFixture(t)
	var queued []int
	for range 3 { // the second is the other event; the third comes after the turn was sent
		if _, err := f.submit(inbound("1700.01")); err != nil {
			t.Fatal(err)
		}
		got := f.drain()
		if queued = append(queued, len(got)); f.woke != len(queued) || (len(got) == 1 && (got[0].UserID != "U1" || got[0].Text != "hello")) {
			t.Fatalf("woke %d times after %d Submits; turn %+v", f.woke, len(queued), got)
		}
	}
	if queued[0] != 1 || queued[1] != 0 || queued[2] != 0 {
		t.Errorf("turns queued by each Submit = %v; want one, then none", queued)
	}
}

// An existing session keeps the Binding it was created under (D14, 8.5) and its threadTTL
// (D28), whatever the caller says. Only a live one is touched: an expired session is
// answered and left exactly as it was, even by an @mention. Exactly threadTTL old is live.
func TestSubmitExistingSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		binding string
		create  bool
		want    Disposition
	}{
		{"live, caller names another Binding", 30 * time.Minute, "ns/b2", true, Accepted},
		{"exactly threadTTL old", time.Hour, "ns/b2", true, Accepted},
		{"a nanosecond older, though ns/b2 would allow it", time.Hour + time.Nanosecond, "ns/b2", true, Expired},
		{"expired, a reply", 2 * time.Hour, "", false, Expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seeded := f.seed("ns/b1", tc.age)
			in := inbound("1700.05")
			in.Binding, in.Create = tc.binding, tc.create
			res, err := f.submit(in)
			got, _ := f.session()
			if tc.want == Accepted {
				seeded.LastActivity = t0 // touched, and nothing else changed
			}
			if err != nil || res.Disposition != tc.want || got != seeded {
				t.Fatalf("Submit = %+v, %v; session %+v; want %s and %+v", res, err, got, tc.want, seeded)
			}
			if queued, want := len(f.drain()), map[Disposition]int{Accepted: 1, Expired: 0}[tc.want]; queued != want || f.woke != want || (tc.want == Expired && f.st.touches != 0) {
				t.Errorf("queued %d, woke %d, %d touches; want %d queued and woken, and no touch for Expired", queued, f.woke, f.st.touches, want)
			}
		})
	}
}

func TestSubmitRefusesWhatCannotBeServed(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Inbound)
		want   error
	}{
		"no message id":      {func(in *Inbound) { in.MessageID = "" }, store.ErrInvalid},
		"the system user":    {func(in *Inbound) { in.UserID = SystemUser }, store.ErrInvalid}, // only intake files a turn under it
		"no text":            {func(in *Inbound) { in.Text = "" }, store.ErrInvalid},
		"create, no Binding": {func(in *Inbound) { in.Binding = "" }, store.ErrInvalid},
		"unknown Binding":    {func(in *Inbound) { in.Binding = "ns/gone" }, ErrUnknownBinding},
	} {
		f, in := newFixture(t), inbound("1700.01")
		tc.mutate(&in)
		_, err := f.submit(in)
		if _, serr := f.session(); !errors.Is(err, tc.want) || !errors.Is(serr, store.ErrNotFound) || f.woke != 0 {
			t.Errorf("%s: Submit = %v, session %v, woke %d; want %v and nothing written", name, err, serr, f.woke, tc.want)
		}
	}
}

// A failed write returns an error and wakes nothing, and the queue handler's retry finishes
// the job (spec 8.6: at-least-once). The touch comes first, so a session that was live when
// the turn was accepted is live for the retry: a retry past the old threadTTL must not turn
// the accepted turn into an expired one that stays queued for ever.
func TestSubmitWriteFailures(t *testing.T) {
	boom := errors.New("disk full")
	for _, tc := range []struct {
		name string
		fail string
		seed bool          // a live session one second from its threadTTL
		wait time.Duration // before the retry
	}{
		{"enqueue after the session was created", "enqueue", false, 2 * time.Second},
		{"enqueue after the session was touched", "enqueue", true, 2 * time.Second},
		{"touch", "touch", true, 0}, // nothing was accepted, so a retry past the boundary is rightly expired
		{"upsert", "upsert", false, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.seed {
				f.seed("ns/b1", time.Hour-time.Second)
			}
			f.st.fail[tc.fail] = boom
			if _, err := f.submit(inbound("1")); !errors.Is(err, boom) || f.woke != 0 {
				t.Fatalf("Submit = %v, woke %d; want the store's error and no wake", err, f.woke)
			}
			f.now.Store(t0.Add(tc.wait).UnixNano())
			res, err := f.submit(inbound("1"))
			if got := len(f.drain()); err != nil || res.Disposition == Expired || got != 1 || f.woke != 1 {
				t.Errorf("retry = %+v, %v; %d turns queued, woke %d; want it accepted once", res, err, got, f.woke)
			}
		})
	}
}

// A runner that is slow to wake must not hold up the intake of other turns.
func TestSubmitWakesOutsideTheIntakeLock(t *testing.T) {
	f := newFixture(t)
	f.seed("ns/b1", 0)
	blocked, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.m.wake = func(Key) {
		if !first.Swap(true) { // only the first wake blocks
			close(blocked)
			<-release
		}
	}
	firstDone := make(chan struct{})
	go func() { _, _ = f.submit(inbound("1")); close(firstDone) }()
	<-blocked
	done := make(chan error, 1)
	go func() { _, err := f.submit(inbound("2")); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a second Submit waited for the first one's wake")
	}
	close(release)
	<-firstDone // the first Submit does not outlive the test
}

// Two Submits that both find no session must not both make one: the second would replace a
// row the first's turn may already have filled in. The gate holds the first read open long
// enough for an unserialized second one to find no session too. The correct path always waits
// the full 200 ms: the second Submit cannot get in while the first holds the lock.
func TestSubmitConcurrentCreate(t *testing.T) {
	f := newFixture(t)
	f.st.gate = make(chan struct{})
	f.m.wake = func(Key) {} // the fixture's counts are not safe for two goroutines
	res := make([]Result, 2)
	var wg sync.WaitGroup
	for i := range res {
		wg.Go(func() {
			var err error
			if res[i], err = f.submit(inbound(fmt.Sprint(i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	created := 0
	for _, r := range res {
		if r.Disposition == Created {
			created++
		}
	}
	if created != 1 || f.st.upserts != 1 || len(f.drain()) != 2 {
		t.Errorf("%d Created, %d session writes; want one of each, and both turns queued", created, f.st.upserts)
	}
}

// A session whose Binding left the config cannot be judged for expiry, so, old or not, it is
// answered with its own disposition, for the caller to tell the thread, and nothing is
// written, queued or woken: there is no retry loop to wait in.
func TestSubmitBindingGone(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed("ns/gone", 100*time.Hour)
	res, err := f.submit(inbound("1700.01"))
	if got, _ := f.session(); err != nil || res.Disposition != BindingGone || got != seeded || f.st.touches != 0 || f.woke != 0 || len(f.drain()) != 0 {
		t.Errorf("Submit = %+v, %v; session %+v, %d touches, woke %d; want BindingGone and no change", res, err, got, f.st.touches, f.woke)
	}
}
