package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

var (
	t0  = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	key = Key{Connection: "slack", Channel: "C1", Thread: "1700.01"}
)

// spyStore counts touches and fails a named call ("touch", "upsert", "enqueue", "set" for
// SetSessionTask, "get" for GetSession or "mark" for MarkReplyDelivered), once or every time, over a real SQLite store.
type spyStore struct {
	Store
	mu         sync.Mutex
	touches    int
	upserts    int
	fail       map[string]error // fails a call once
	stuck      map[string]error // fails a call every time
	onSet      func()           // runs when a task is cleared
	onClearAck func()           // runs before ClearSessionAcknowledgment updates the active task turn
	onNext     func()           // runs before NextReply reads the queue
	gate       chan struct{}    // when set, the first GetSession waits for a second, or 200 ms
	reads      int
}

func (s *spyStore) GetSession(ctx context.Context, k Key, ttl time.Duration, now time.Time) (store.Session, error) {
	s.mu.Lock()
	s.reads++
	n := s.reads
	s.mu.Unlock()
	if err := s.failOnce("get"); err != nil {
		return store.Session{}, err
	}
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

func (s *spyStore) SetSessionTask(ctx context.Context, k Key, contextID, taskID string, turn store.TaskTurn) error {
	s.mu.Lock()
	hook := s.onSet
	s.mu.Unlock()
	if hook != nil && taskID == "" { // a task being cleared
		hook()
	}
	if err := s.failOnce("set"); err != nil {
		return err
	}
	return s.Store.SetSessionTask(ctx, k, contextID, taskID, turn)
}

func (s *spyStore) ClearSessionAcknowledgment(ctx context.Context, k Key, taskID, turnID string) error {
	s.mu.Lock()
	hook := s.onClearAck
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Store.ClearSessionAcknowledgment(ctx, k, taskID, turnID)
}

func (s *spyStore) NextReply(ctx context.Context, k Key) (store.Reply, error) {
	s.mu.Lock()
	hook := s.onNext
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Store.NextReply(ctx, k)
}

func (s *spyStore) ResetSession(ctx context.Context, k Key) error {
	if err := s.failOnce("reset"); err != nil {
		return err
	}
	return s.Store.ResetSession(ctx, k)
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
	replyMu          sync.RWMutex
	textReply        func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error)
	t                *testing.T
	st               *spyStore
	raw              *sqlite.Store // the store under st, for what the manager does not write itself: interactions
	qs               *spyQuestions // the manager's Questions
	m                *Manager
	woke             int
	onWake           func()
	now              atomic.Int64 // the clock, in Unix nanoseconds
	agent            *fakeAgent
	real             AgentAPI // when set, the agent the Binding uses instead of the fake
	amu              sync.Mutex
	rejected, audits []audit.Call  // what Rejected and the Auditor were given
	actors           []audit.Actor // who the Auditor's calls named
	attr             func(*a2a.Message, dialect.User)
	platform         dialect.Platform // the Backend's chat platform
	bindingErr       error            // when set, every Binding lookup fails with it (guarded by amu)
	noRejected       bool             // the Backend has no identity Source to call Rejected on
	textReplies      bool
	feedback         bool // the Backend has feedback.enabled
	output           v1alpha1.OutputMode
	progress         v1alpha1.ProgressMode
	threadReplies    v1alpha1.ThreadReplies // the Binding's chat.threadReplies; empty is all
	ackConfigured    bool
	ackEnabled       bool
	ackEmoji         string
}

func (f *fixture) reject(_ context.Context, _ audit.Actor, c audit.Call) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.rejected = append(f.rejected, c)
	return nil
}

// AgentCall makes the fixture the Auditor.
func (f *fixture) AgentCall(_ context.Context, a audit.Actor, c audit.Call) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.audits, f.actors = append(f.audits, c), append(f.actors, a)
	return nil
}

// Backend makes the fixture the Bindings: ns/b1 has a threadTTL of 1 h, ns/b2 of 1000 h.
func (f *fixture) Backend(name string) (Backend, error) {
	f.amu.Lock()
	err := f.bindingErr
	f.amu.Unlock()
	if err != nil {
		return Backend{}, err
	}
	ttl, ok := map[string]time.Duration{"ns/b1": time.Hour, "ns/b2": 1000 * time.Hour}[name]
	if !ok {
		return Backend{}, ErrUnknownBinding
	}
	var ag AgentAPI = f.agent
	if f.real != nil {
		ag = f.real
	}
	b := Backend{AgentName: "kagent/agent-1", Dialect: f.dialect(), ThreadTTL: ttl, Agent: ag, Attribute: f.attr, Platform: f.platform, Rejected: f.reject, Identity: "secret:ns/token", Output: f.output, Progress: f.progress, Feedback: f.feedback, ThreadReplies: f.threadReplies, AcknowledgmentConfigured: f.ackConfigured, AcknowledgmentEnabled: f.ackEnabled, AcknowledgmentEmoji: f.ackEmoji}
	if f.noRejected {
		b.Rejected = nil
	}
	return b, nil
}

// dialect is the Binding's: a2a has questions that take text replies, kagent-v1 has none.
func (f *fixture) dialect() string {
	if f.textReplies {
		return string(v1alpha1.DialectA2A)
	}
	return string(v1alpha1.DialectKagentV1)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, st: &spyStore{Store: s, fail: map[string]error{}, stuck: map[string]error{}}, raw: s, qs: &spyQuestions{Questions: s}, agent: newFakeAgent()}
	f.now.Store(t0.UnixNano())
	f.m, err = New(Config{Store: f.st, Questions: f.qs, TextReply: f.decideTextReply, Bindings: f, Audit: f, Users: idOnly{}, Sink: quietSink{}, ThreadContext: NoThreadContext, OnTurnEnd: noTurnEnd, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return time.Unix(0, f.now.Load()).UTC() }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.m.wake = func(Key) { f.onWake(); f.woke++ }
	f.onWake = func() {}
	return f
}

// quietSink renders nothing: a turn has no handler, and a notice is dropped.
type quietSink struct{}

func (quietSink) Turn(context.Context, TurnRef) agent.Handler { return nil }
func (quietSink) Notice(context.Context, Key, Notice)         {}

// idOnly resolves every user to their ID alone, which is what a turn is attributed by when the platform knows no one.
type idOnly struct{}

func (idOnly) ResolveUser(_ context.Context, id string) (dialect.User, error) {
	return dialect.User{ID: id}, nil
}

// noQuestions finds no pending question in any thread.
type noQuestions struct{}

func (noQuestions) PendingTextQuestion(context.Context, store.ConversationKey) (store.Interaction, error) {
	return store.Interaction{}, store.ErrNotFound
}

func noTurnEnd(string, TurnOutcome) {}

// spyQuestions counts the reads of the manager's Questions and fails them when err is set (a store error on the
// intake path).
type spyQuestions struct {
	Questions
	mu    sync.Mutex
	err   error
	calls int
}

func (q *spyQuestions) PendingTextQuestion(ctx context.Context, k store.ConversationKey) (store.Interaction, error) {
	q.mu.Lock()
	q.calls++
	err := q.err
	q.mu.Unlock()
	if err != nil {
		return store.Interaction{}, err
	}
	return q.Questions.PendingTextQuestion(ctx, k)
}

// question makes the pending interaction id of the thread, which takes text replies when text is set, with its
// prompt posted.
func (f *fixture) question(id string, text bool) store.Interaction {
	f.t.Helper()
	i := f.unpostedQuestion(id, text)
	if err := f.raw.SetInteractionMessageRef(f.t.Context(), id, "1700.50"); err != nil {
		f.t.Fatal(err)
	}
	i.MessageRef = "1700.50"
	return i
}

// unpostedQuestion is question for one whose prompt has not been posted: nobody has seen it yet.
func (f *fixture) unpostedQuestion(id string, text bool) store.Interaction {
	f.t.Helper()
	i := store.Interaction{ID: id, Key: key, TaskID: "t1", Kind: store.InteractionAskUser, TextReplies: text, CreatedAt: t0, Deadline: t0.Add(time.Hour)}
	if _, err := f.raw.CreateInteraction(f.t.Context(), i); err != nil {
		f.t.Fatal(err)
	}
	i.State = store.InteractionPending
	return i
}

// inbound is the mention in Slack message ts that may create the session under ns/b1.
func inbound(ts string) Inbound {
	return Inbound{Key: key, Binding: "ns/b1", MessageID: "slack:slack:C1:" + ts, MessageRef: ts, UserID: "U1", Text: "hello", Create: true}
}

// submit queues a turn the test expects to be taken: a Submit that answers anything but Created
// or Accepted, with no error, fails the test, so that a turn refused in silence (an expired thread,
// a thread nobody owns) cannot hide behind a test that waits for what it would have caused.
func (f *fixture) submit(in Inbound) (Result, error) {
	f.t.Helper()
	res, err := f.submitAny(in)
	if err == nil && res.Disposition != Created && res.Disposition != Accepted {
		f.t.Errorf("submit %s = %+v; want the turn taken", in.MessageID, res) // Errorf: a test may call it off its own goroutine
	}
	return res, err
}

// submitAny is submit for a test that expects a turn to be refused, or checks what Submit says.
func (f *fixture) submitAny(in Inbound) (Result, error) { return f.m.Submit(f.t.Context(), in) }

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
	res, err := f.submitAny(in)
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
		if r, err := f.st.NextReply(t.Context(), key); err != nil || r.ID != "slack:slack:C1:1700.01" || r.UserID != SystemUser || r.Text != "hello" || r.MessageRef != "1700.01" {
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
			res, err := f.submitAny(in)
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

// An expired session is kept, so that a reply in its thread is told so (D28); once retention has deleted it (DP-3), the
// thread is one that Agent Kourier never owned: a reply is ignored, writing and waking nothing, and an @mention starts a new
// session anchored to the thread, as it does in any thread Agent Kourier does not own (8.2).
func TestSubmitInAThreadWhoseSessionWasDeleted(t *testing.T) {
	f := newFixture(t)
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, ContextID: "c1", Binding: "ns/b1", LastActivity: t0.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	reply := inbound("1700.05")
	reply.Create = false
	if res, err := f.submitAny(reply); err != nil || res.Disposition != Expired {
		t.Fatalf("a reply in the expired thread = %+v, %v; want Expired, the notice", res, err)
	}

	// The horizon is the session's threadTTL (1 h) plus the grace, here 30 minutes: it was last active 2 h ago.
	horizon := t0.Add(-90 * time.Minute)
	purged, err := f.st.Store.(store.SessionRetention).PurgeExpiredSessions(t.Context(), store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b1": horizon}, Otherwise: horizon}, 10)
	if err != nil || purged.Sessions != 1 {
		t.Fatalf("PurgeExpiredSessions = %+v, %v; want the session", purged, err)
	}

	reply = inbound("1700.06")
	reply.Create = false
	res, err := f.submitAny(reply)
	if _, serr := f.session(); err != nil || res.Disposition != NotOwned || !errors.Is(serr, store.ErrNotFound) || f.woke != 0 || len(f.drain()) != 0 {
		t.Fatalf("a reply in the swept thread = %+v, %v; session %v, woke %d; want NotOwned and nothing written or woken", res, err, serr, f.woke)
	}
	if res, err := f.submit(inbound("1700.07")); err != nil || res.Disposition != Created {
		t.Errorf("an @mention in the swept thread = %+v, %v; want a new session", res, err)
	}
}

func TestSubmitRefusesWhatCannotBeServed(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Inbound)
		want   error
	}{
		"no message id":             {func(in *Inbound) { in.MessageID = "" }, store.ErrInvalid},
		"a recovered turn's prefix": {func(in *Inbound) { in.MessageID = "task:t1" }, store.ErrInvalid}, // reserved for recovery
		"the system user":           {func(in *Inbound) { in.UserID = SystemUser }, store.ErrInvalid},   // only intake files a turn under it
		"no text":                   {func(in *Inbound) { in.Text = "" }, store.ErrInvalid},
		"create, no Binding":        {func(in *Inbound) { in.Binding = "" }, store.ErrInvalid},
		"unknown Binding":           {func(in *Inbound) { in.Binding = "ns/gone" }, ErrUnknownBinding},
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
	res, err := f.submitAny(inbound("1700.01"))
	if got, _ := f.session(); err != nil || res.Disposition != BindingGone || got != seeded || f.st.touches != 0 || f.woke != 0 || len(f.drain()) != 0 {
		t.Errorf("Submit = %+v, %v; session %+v, %d touches, woke %d; want BindingGone and no change", res, err, got, f.st.touches, f.woke)
	}
}

// A Binding lookup that fails for any reason but ErrUnknownBinding (a config reload, say) is an
// error, not a removed Binding: nothing is written, queued or woken, and the queue handler
// retries.
func TestSubmitTransientBindingErrorIsNotBindingGone(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed("ns/b1", 0)
	reload := errors.New("config reload in progress")
	f.amu.Lock()
	f.bindingErr = reload
	f.amu.Unlock()
	res, err := f.submit(inbound("1700.01"))
	if got, _ := f.session(); !errors.Is(err, reload) || res.Disposition == BindingGone || got != seeded || f.st.touches != 0 || f.woke != 0 || len(f.drain()) != 0 {
		t.Errorf("Submit = %+v, %v; want the lookup's own error and nothing written", res, err)
	}
}

// reply is a person's reply in the thread, as the chat intake submits one: it creates nothing.
func reply(ts string, mentioned bool) Inbound {
	in := inbound(ts)
	in.Create, in.Mentioned = false, mentioned
	return in
}

// TestSubmitThreadReplyGate is the intake gate (spec 8.2, D38) over the Binding's mode, whether the
// reply mentions Agent Kourier, and the state of the thread. In mention mode a reply that does not mention
// Agent Kourier is Unmentioned: nothing is written, queued, touched or woken, whether the thread is live or
// expired (so no expiry notice either). Everything else is as it is in all mode.
func TestSubmitThreadReplyGate(t *testing.T) {
	const (
		live    = 0 * time.Hour
		expired = 2 * time.Hour // past ns/b1's threadTTL of 1 h
	)
	tests := []struct {
		name      string
		mode      v1alpha1.ThreadReplies
		age       time.Duration
		mentioned bool
		want      Disposition
	}{
		{"mention: unmentioned in a live thread", v1alpha1.ThreadRepliesMention, live, false, Unmentioned},
		{"mention: mentioned in a live thread", v1alpha1.ThreadRepliesMention, live, true, Accepted},
		{"mention: unmentioned in an expired thread", v1alpha1.ThreadRepliesMention, expired, false, Unmentioned},
		{"mention: mentioned in an expired thread", v1alpha1.ThreadRepliesMention, expired, true, Expired},
		{"all: unmentioned in a live thread", v1alpha1.ThreadRepliesAll, live, false, Accepted},
		{"all: mentioned in a live thread", v1alpha1.ThreadRepliesAll, live, true, Accepted},
		{"all: unmentioned in an expired thread", v1alpha1.ThreadRepliesAll, expired, false, Expired},
		{"all: mentioned in an expired thread", v1alpha1.ThreadRepliesAll, expired, true, Expired},
		{"unset mode is all: unmentioned in a live thread", "", live, false, Accepted},
		{"unset mode is all: unmentioned in an expired thread", "", expired, false, Expired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.threadReplies = tt.mode
			seeded := f.seed("ns/b1", tt.age)
			res, err := f.submitAny(reply("1700.02", tt.mentioned))
			if err != nil || res.Disposition != tt.want {
				t.Fatalf("Submit = %+v, %v; want %s", res, err, tt.want)
			}
			// The caller acts on the admission: a turn is remembered, a notice posted, the rest left alone.
			kinds := map[Disposition]AdmissionKind{Accepted: AdmitTurn, Unmentioned: AdmitIgnored, Expired: AdmitNotice}
			if res.Admission.Kind != kinds[tt.want] {
				t.Errorf("Admission = %+v; want kind %s", res.Admission, kinds[tt.want])
			}
			got, _ := f.session()
			switch tt.want {
			case Accepted:
				if f.st.touches != 1 || !got.LastActivity.Equal(t0) || f.woke != 1 || len(f.drain()) != 1 {
					t.Errorf("accepted: %d touches, LastActivity %v, woke %d; want one touch to %v, one wake and one queued turn", f.st.touches, got.LastActivity, f.woke, t0)
				}
			default:
				if got != seeded || f.st.touches != 0 || f.st.upserts != 1 || f.woke != 0 || len(f.drain()) != 0 {
					t.Errorf("%s: session %+v (seeded %+v), %d touches, %d upserts, woke %d; want nothing written, queued or woken", tt.want, got, seeded, f.st.touches, f.st.upserts, f.woke)
				}
			}
		})
	}
}

// A reply in a thread whose Binding is gone is answered with the Binding-gone notice, or left alone, as the mode of the
// Binding that owns the channel now says (spec 8.2): the session's own Binding cannot be asked. Either way nothing is
// written, queued, touched or woken, and a reply left alone is not counted as unmentioned: no Binding is left to count
// it for.
func TestSubmitBindingGoneFollowsTheModeOfTheChannelsBinding(t *testing.T) {
	tests := []struct {
		name      string
		channel   v1alpha1.ThreadReplies // the Binding that owns the channel now; empty is none, or all
		mentioned bool
		want      Admission
	}{
		{"no owner", "", false, Admission{Kind: AdmitNotice, Notice: NoticeBindingGone}},
		{"the owner takes all", v1alpha1.ThreadRepliesAll, false, Admission{Kind: AdmitNotice, Notice: NoticeBindingGone}},
		{"the owner takes mentions, a plain reply", v1alpha1.ThreadRepliesMention, false, Admission{Kind: AdmitIgnored, Ignored: IgnoredBindingGone}},
		{"the owner takes mentions, a mentioned reply", v1alpha1.ThreadRepliesMention, true, Admission{Kind: AdmitNotice, Notice: NoticeBindingGone}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ctx, _, reg := tracingFixture(t)
			f.threadReplies = v1alpha1.ThreadRepliesAll // the session's own Binding is gone: this is never asked
			seeded := f.seed("ns/gone", 100*time.Hour)
			in := reply("1700.02", tt.mentioned)
			in.ChannelThreadReplies = tt.channel
			res, err := f.m.Submit(ctx, in)
			if got, _ := f.session(); err != nil || res.Disposition != BindingGone || res.Admission != tt.want || got != seeded || f.st.touches != 0 || f.woke != 0 || len(f.drain()) != 0 {
				t.Errorf("Submit = %+v, %v; session %+v, %d touches, woke %d; want BindingGone, %+v and no change", res, err, got, f.st.touches, f.woke, tt.want)
			}
			if got := counter(t, reg, "agentkourier_thread_unmentioned_total", map[string]string{"binding": "ns/gone"}); got != 0 {
				t.Errorf("agentkourier_thread_unmentioned_total = %v; want 0", got)
			}
		})
	}
}

// A reply that does not mention Agent Kourier is taken in mention mode when the thread has a pending question that takes
// text replies: it is that question's answer, enqueued already pinned to it, so it is never an ordinary turn
// (decision 3). Any other pending question, or none, leaves it Unmentioned.
func TestSubmitUnmentionedReplyToATextQuestion(t *testing.T) {
	const live, expired = 0 * time.Hour, 2 * time.Hour
	tests := []struct {
		name      string
		mode      v1alpha1.ThreadReplies
		age       time.Duration
		question  string // "text", "buttons", "unposted" (a text question whose prompt is not posted) or "" for none
		task      string // the session's recorded task: "" is the question's (t1), "none" or "other"
		kagent    bool   // the Binding's dialect has no text questions
		mentioned bool
		want      Disposition
		asked     bool // the manager read the question
	}{
		{"mention: a pending text question", v1alpha1.ThreadRepliesMention, live, "text", "", false, false, Accepted, true},
		{"mention: a pending question that takes buttons only", v1alpha1.ThreadRepliesMention, live, "buttons", "", false, false, Unmentioned, true},
		{"mention: a text question nobody has seen yet", v1alpha1.ThreadRepliesMention, live, "unposted", "", false, false, Unmentioned, true},
		{"mention: a text question the session has no task for", v1alpha1.ThreadRepliesMention, live, "text", "none", false, false, Unmentioned, true},
		{"mention: a text question of a task that is not the session's", v1alpha1.ThreadRepliesMention, live, "text", "other", false, false, Unmentioned, true},
		{"mention: no question", v1alpha1.ThreadRepliesMention, live, "", "", false, false, Unmentioned, true},
		{"mention: a dialect with no text questions is not asked", v1alpha1.ThreadRepliesMention, live, "text", "", true, false, Unmentioned, false},
		{"mention: a pending text question in an expired thread", v1alpha1.ThreadRepliesMention, expired, "text", "", false, false, Unmentioned, false},
		{"mention: a mentioned reply is no answer candidate here and is not pinned", v1alpha1.ThreadRepliesMention, live, "text", "", false, true, Accepted, false},
		{"all: the question is not asked", v1alpha1.ThreadRepliesAll, live, "text", "", false, false, Accepted, false},
		{"unset mode is all: the question is not asked", "", live, "text", "", false, false, Accepted, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.threadReplies, f.textReplies = tt.mode, !tt.kagent
			f.seed("ns/b1", tt.age)
			var q store.Interaction
			switch tt.question {
			case "text", "buttons":
				q = f.question("q1", tt.question == "text")
			case "unposted":
				q = f.unpostedQuestion("q1", true)
			}
			switch tt.task {
			case "none":
				f.setTask("")
			case "other":
				f.setTask("t2")
			}
			res, err := f.submitAny(reply("1700.02", tt.mentioned))
			if err != nil || res.Disposition != tt.want {
				t.Fatalf("Submit = %+v, %v; want %s", res, err, tt.want)
			}
			if got := f.qs.calls > 0; got != tt.asked {
				t.Errorf("the question was read = %v; want %v", got, tt.asked)
			}
			queued := f.drain()
			if tt.want != Accepted {
				if len(queued) != 0 || f.st.touches != 0 || f.woke != 0 {
					t.Errorf("%d queued, %d touches, woke %d; want nothing written or woken", len(queued), f.st.touches, f.woke)
				}
				return
			}
			pinned := ""
			if tt.mode == v1alpha1.ThreadRepliesMention && !tt.mentioned {
				pinned = q.ID
			}
			if wantKind := map[bool]AdmissionKind{true: AdmitAnswer, false: AdmitTurn}[pinned != ""]; res.Admission.Kind != wantKind || res.Admission.Question != pinned {
				t.Errorf("Admission = %+v; want kind %s for the question %q", res.Admission, wantKind, pinned)
			}
			if len(queued) != 1 || queued[0].InteractionID != pinned || f.st.touches != 1 || f.woke != 1 {
				t.Errorf("queued %+v, %d touches, woke %d; want one reply pinned to %q, one touch and one wake", queued, f.st.touches, f.woke, pinned)
			}
		})
	}
}

// setTask records task as the session's active task (empty: none), as the runner does when a task begins or ends.
func (f *fixture) setTask(task string) {
	f.t.Helper()
	if err := f.raw.SetSessionTask(f.t.Context(), key, "c1", task, store.TaskTurn{}); err != nil {
		f.t.Fatal(err)
	}
}

func TestSubmitUnmentionedReplyToATextQuestionEdges(t *testing.T) {
	t.Run("a question the store cannot read fails the submit, writing nothing, so that Slack redelivers", func(t *testing.T) {
		f := newFixture(t)
		f.threadReplies, f.textReplies = v1alpha1.ThreadRepliesMention, true
		f.seed("ns/b1", 0)
		f.question("q1", true)
		boom := errors.New("database is locked")
		f.qs.err = boom
		if res, err := f.submitAny(reply("1700.02", false)); !errors.Is(err, boom) || res.Disposition != "" {
			t.Fatalf("Submit = %+v, %v; want the store's error and no disposition", res, err)
		}
		if f.st.touches != 0 || f.woke != 0 || len(f.drain()) != 0 {
			t.Error("a failed read left a touch, a wake or a queued reply")
		}
		f.qs.err = nil // the redelivery
		if res, err := f.submitAny(reply("1700.02", false)); err != nil || res.Disposition != Accepted {
			t.Fatalf("redelivered Submit = %+v, %v; want Accepted", res, err)
		}
	})
	t.Run("no pending question found: every unmentioned reply is ignored", func(t *testing.T) {
		f := newFixture(t)
		f.m.cfg.Questions = noQuestions{}
		f.threadReplies, f.textReplies = v1alpha1.ThreadRepliesMention, true
		f.seed("ns/b1", 0)
		f.question("q1", true)
		if res, err := f.submitAny(reply("1700.02", false)); err != nil || res.Disposition != Unmentioned {
			t.Fatalf("Submit = %+v, %v; want Unmentioned", res, err)
		}
	})
	t.Run("a pinned answer is not counted as unmentioned", func(t *testing.T) {
		f, ctx, _, reg := tracingFixture(t)
		f.threadReplies, f.textReplies = v1alpha1.ThreadRepliesMention, true
		f.seed("ns/b1", 0)
		f.question("q1", true)
		if _, err := f.m.Submit(ctx, reply("1700.02", false)); err != nil {
			t.Fatal(err)
		}
		if got := counter(t, reg, "agentkourier_thread_unmentioned_total", map[string]string{"binding": "ns/b1"}); got != 0 {
			t.Errorf("agentkourier_thread_unmentioned_total = %v; want 0: a typed answer is a turn of the question", got)
		}
	})
	t.Run("a redelivered answer is one reply, still pinned", func(t *testing.T) {
		f := newFixture(t)
		f.threadReplies, f.textReplies = v1alpha1.ThreadRepliesMention, true
		f.seed("ns/b1", 0)
		f.question("q1", true)
		for range 2 {
			if res, err := f.submitAny(reply("1700.02", false)); err != nil || res.Disposition != Accepted {
				t.Fatalf("Submit = %+v, %v; want Accepted", res, err)
			}
		}
		if got := f.drain(); len(got) != 1 || got[0].InteractionID != "q1" {
			t.Errorf("queued = %+v; want one reply pinned to q1", got)
		}
	})
}

// The gate counts what it turns away, by the session's Binding: the engagement of a thread that people
// are talking in without waking the agent.
func TestSubmitCountsUnmentionedReplies(t *testing.T) {
	f, ctx, _, reg := tracingFixture(t)
	f.threadReplies = v1alpha1.ThreadRepliesMention
	f.seed("ns/b1", 0)
	for _, in := range []Inbound{reply("1700.02", false), reply("1700.03", false), reply("1700.04", true)} {
		if _, err := f.m.Submit(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if got := counter(t, reg, "agentkourier_thread_unmentioned_total", map[string]string{"binding": "ns/b1"}); got != 2 {
		t.Errorf("agentkourier_thread_unmentioned_total{binding=ns/b1} = %v; want 2: the mentioned reply is a turn, not counted", got)
	}
}

// counter is the value of the counter name in reg with exactly these labels, and 0 if it has none.
func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.Metric {
			got := map[string]string{}
			for _, l := range m.Label {
				got[l.GetName()] = l.GetValue()
			}
			if maps.Equal(got, labels) {
				return m.Counter.GetValue()
			}
		}
	}
	return 0
}

// Disposition and Admission are two names of one outcome, and admit is the one place that pairs them: for every kind of
// outcome a Submit can have, they agree (the chat intake and the metrics read one each).
func TestSubmitDispositionAndAdmissionAgree(t *testing.T) {
	const expired = 2 * time.Hour // past ns/b1's threadTTL of 1 h
	tests := []struct {
		name  string
		setup func(f *fixture)
		in    Inbound
		want  Result
	}{
		{"created", func(*fixture) {}, inbound("1700.02"), Result{Disposition: Created, Admission: Admission{Kind: AdmitTurn}}},
		{"accepted", func(f *fixture) { f.seed("ns/b1", 0) }, inbound("1700.02"), Result{Disposition: Accepted, Admission: Admission{Kind: AdmitTurn}}},
		{"answer", func(f *fixture) {
			f.threadReplies, f.textReplies = v1alpha1.ThreadRepliesMention, true
			f.seed("ns/b1", 0)
			f.question("q1", true)
		}, reply("1700.02", false), Result{Disposition: Accepted, Admission: Admission{Kind: AdmitAnswer, Question: "q1"}}},
		{"not owned", func(*fixture) {}, reply("1700.02", false), Result{Disposition: NotOwned, Admission: Admission{Kind: AdmitIgnored, Ignored: IgnoredNotOwned}}},
		{"unmentioned", func(f *fixture) { f.threadReplies = v1alpha1.ThreadRepliesMention; f.seed("ns/b1", 0) }, reply("1700.02", false), Result{Disposition: Unmentioned, Admission: Admission{Kind: AdmitIgnored, Ignored: IgnoredUnmentioned}}},
		{"unmentioned in an expired thread", func(f *fixture) { f.threadReplies = v1alpha1.ThreadRepliesMention; f.seed("ns/b1", expired) }, reply("1700.02", false), Result{Disposition: Unmentioned, Admission: Admission{Kind: AdmitIgnored, Ignored: IgnoredUnmentioned}}},
		{"expired", func(f *fixture) { f.seed("ns/b1", expired) }, reply("1700.02", true), Result{Disposition: Expired, Admission: Admission{Kind: AdmitNotice, Notice: NoticeExpired}}},
		{"binding gone", func(f *fixture) { f.seed("ns/gone", expired) }, reply("1700.02", true), Result{Disposition: BindingGone, Admission: Admission{Kind: AdmitNotice, Notice: NoticeBindingGone}}},
		{"binding gone, left alone", func(f *fixture) { f.seed("ns/gone", expired) }, func() Inbound {
			in := reply("1700.02", false)
			in.ChannelThreadReplies = v1alpha1.ThreadRepliesMention
			return in
		}(), Result{Disposition: BindingGone, Admission: Admission{Kind: AdmitIgnored, Ignored: IgnoredBindingGone}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			res, err := f.submitAny(tt.in)
			res.Queued = false // not part of the pairing
			if err != nil || res != tt.want {
				t.Errorf("Submit = %+v, %v; want %+v", res, err, tt.want)
			}
		})
	}
}

// A kind or reason the manager does not know is an error that writes nothing, never a turn.
func TestCarryOutRefusesAnAdmissionItDoesNotKnow(t *testing.T) {
	for _, adm := range []Admission{
		{},
		{Kind: "reject"},
		{Kind: AdmitIgnored},
		{Kind: AdmitIgnored, Ignored: "shrug"},
		{Kind: AdmitNotice},
		{Kind: AdmitNotice, Notice: NoticeQueued},
	} {
		t.Run(string(adm.Kind)+"/"+string(adm.Ignored)+string(adm.Notice), func(t *testing.T) {
			f := newFixture(t)
			seeded := f.seed("ns/b1", 0)
			res, pin, err := f.m.carryOut(t.Context(), reply("1700.02", true), true, "ns/b1", adm, t0)
			if got, _ := f.session(); err == nil || res != (Result{}) || pin != "" || got != seeded || f.st.touches != 0 || f.st.upserts != 1 || len(f.drain()) != 0 {
				t.Errorf("carryOut(%+v) = %+v, %q, %v; session %+v, %d touches; want an error and nothing written", adm, res, pin, err, got, f.st.touches)
			}
		})
	}
}

// setTextReply changes the fake decision, not the manager's constructed dependency. The mutex
// only guards the fake; calls run outside it so a decision can re-enter Manager.Answer or Cancel.
func (f *fixture) setTextReply(h func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error)) {
	f.replyMu.Lock()
	f.textReply = h
	f.replyMu.Unlock()
}

func (f *fixture) decideTextReply(ctx context.Context, key Key, reply store.Reply, task a2a.Task) (TextReplyOutcome, error) {
	f.replyMu.RLock()
	h := f.textReply
	f.replyMu.RUnlock()
	if h == nil {
		return NoTextReply(ctx, key, reply, task)
	}
	return h(ctx, key, reply, task)
}
