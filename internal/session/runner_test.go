package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// fakeAgent is an AgentAPI whose turns a test scripts: each Send announces its messageId on
// sent, then runs script with its 0-based call number.
type fakeAgent struct {
	mu                sync.Mutex
	msgs              []*a2a.Message
	active, maxActive int // Sends running now, and at most
	sent              chan string
	script            func(ctx context.Context, n int, h agent.Handler) error
	// find and follow answer FindTask and Follow; finds and follows record their calls.
	find    func(contextID, messageID string) (*a2a.Task, []a2a.TaskID, error)
	follow  func(id a2a.TaskID, h agent.Handler) error
	finds   []string
	follows []a2a.TaskID
}

func (a *fakeAgent) FindTask(_ context.Context, contextID, messageID string) (*a2a.Task, []a2a.TaskID, error) {
	a.mu.Lock()
	a.finds = append(a.finds, contextID+" "+messageID)
	find := a.find
	a.mu.Unlock()
	if find == nil {
		return nil, nil, nil
	}
	return find(contextID, messageID)
}

func (a *fakeAgent) Follow(_ context.Context, id a2a.TaskID, h agent.Handler) error {
	a.mu.Lock()
	a.follows = append(a.follows, id)
	a.mu.Unlock()
	return a.follow(id, h)
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sent: make(chan string, 16), script: func(_ context.Context, n int, h agent.Handler) error {
		return turn(h, n, a2a.TaskStateCompleted)
	}}
}

func (a *fakeAgent) Send(ctx context.Context, msg *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	n := len(a.msgs)
	a.msgs = append(a.msgs, msg)
	a.active++
	a.maxActive = max(a.maxActive, a.active)
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.active--; a.mu.Unlock() }()
	a.sent <- msg.ID
	return a.script(ctx, n, h)
}

// accepted is what an agent streams when it takes turn n: the task, in context c1.
func accepted(h agent.Handler, n int) a2a.TaskID {
	id := a2a.TaskID(fmt.Sprintf("t%d", n+1))
	h(agent.StateEvent{TaskID: id, ContextID: "c1", State: a2a.TaskStateWorking})
	return id
}

// turn accepts turn n and ends it in state.
func turn(h agent.Handler, n int, state a2a.TaskState) error {
	id := accepted(h, n)
	h(agent.FinalEvent{Task: &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: state}}})
	return nil
}

// fakeSink records every event and notice, and announces each FinalEvent on finals.
type fakeSink struct {
	mu      sync.Mutex
	events  []agent.Event
	notices []Notice
	finals  chan struct{}
}

func (s *fakeSink) Notice(_ context.Context, _ Key, n Notice) {
	s.mu.Lock()
	s.notices = append(s.notices, n)
	s.mu.Unlock()
}

func (s *fakeSink) Turn(context.Context, TurnRef) agent.Handler {
	return func(e agent.Event) {
		s.mu.Lock()
		s.events = append(s.events, e)
		s.mu.Unlock()
		if _, ok := e.(agent.FinalEvent); ok {
			s.finals <- struct{}{}
		}
	}
}

type fakeUsers map[string]dialect.User

func (u fakeUsers) ResolveUser(_ context.Context, id string) (dialect.User, error) {
	if user, ok := u[id]; ok {
		return user, nil
	}
	return dialect.User{}, errors.New("no such user")
}

// live starts real runners on Submit, with a sink, and returns the sink.
func (f *fixture) live() *fakeSink {
	sink := &fakeSink{finals: make(chan struct{}, 16)}
	f.m.cfg.Sink, f.m.wake = sink, f.m.start
	f.t.Cleanup(func() { _ = f.close() })
	return sink
}

// close closes the manager, failing the test if it does not return in 5 s.
func (f *fixture) close() error {
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	return f.m.Close(ctx)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (f *fixture) taskID() string { sess, _ := f.session(); return sess.TaskID }

// A turn is one text part (R18) carrying the queue's messageId, and the session's contextId,
// which is empty on the first turn.
func TestTurnMessage(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	for i, wantContext := range []string{"", "c1"} {
		in := inbound(fmt.Sprint(i))
		if _, err := f.submit(in); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
		f.agent.mu.Lock()
		msg := f.agent.msgs[i]
		f.agent.mu.Unlock()
		if len(msg.Parts) != 1 || msg.Parts[0].Text() != "hello" || msg.ID != in.MessageID || msg.ContextID != wantContext {
			t.Errorf("turn %d = %d parts, id %q, context %q; want one part, id %q, context %q", i, len(msg.Parts), msg.ID, msg.ContextID, in.MessageID, wantContext)
		}
	}
}

// A turn carries who wrote it (D14), inside its one text part (R18): the user the platform
// resolves, else the ID alone, and nothing for a turn nobody wrote.
func TestTurnAttribution(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.attr = (&kagentv1.Dialect{}).Attribute
	var logs bytes.Buffer // written by the runner before it sends, read after the turn ended
	f.m.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	f.m.cfg.Users = fakeUsers{"U1": {ID: "U1", Name: "Ada", Email: "ada@example.com"}, "U3": {Name: "Eve"}}
	for i, tc := range []struct{ user, want string }{
		{"U1", "[Slack: Ada (U1) <ada@example.com>]\nhello"},
		{"U2", "[Slack: U2]\nhello"},       // the platform cannot resolve U2
		{"U3", "[Slack: Eve (U3)]\nhello"}, // the platform's answer names no ID: the author's stays
		{"", "hello"},                      // an alert's prompt
	} {
		in := inbound(fmt.Sprint(i))
		in.UserID = tc.user
		if _, err := f.submit(in); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
		f.agent.mu.Lock()
		msg := f.agent.msgs[i]
		f.agent.mu.Unlock()
		if len(msg.Parts) != 1 || msg.Parts[0].Text() != tc.want {
			t.Errorf("turn %d = %+v; want one part %q", i, msg.Parts, tc.want)
		}
	}
	if got := logs.String(); !strings.Contains(got, "U2") || !strings.Contains(got, "no such user") {
		t.Errorf("log = %q; want the failed resolution of U2 and its cause", got)
	}
}

// The first StateEvent names the task: the session then holds contextId and task, and the
// turn has left the queue, while the stream still runs (spec 8.2). When the task ends the
// task is cleared, and the session touched.
func TestAcceptedTurnIsRecordedMidStream(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	mid, gate := make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		mid <- struct{}{}
		<-gate
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-mid
	sess, err := f.session()
	if _, nerr := f.st.NextReply(t.Context(), key); err != nil || sess.ContextID != "c1" || sess.TaskID != "t1" || !errors.Is(nerr, store.ErrNotFound) {
		t.Fatalf("mid-stream: session %+v, %v; queue %v; want c1 and t1, and the turn delivered", sess, err, nerr)
	}
	f.now.Store(t0.Add(time.Minute).UnixNano())
	close(gate)
	<-sink.finals
	eventually(t, "the task cleared and the session touched", func() bool {
		sess, _ := f.session()
		return sess.TaskID == "" && sess.ContextID == "c1" && sess.LastActivity.Equal(t0.Add(time.Minute))
	})
}

// A task that waits for input parks the conversation: the next turn stays queued and the
// task stays recorded, for recovery (spec 8.4).
func TestInputRequiredParksTheConversation(t *testing.T) {
	f := newFixture(t)
	ignore := goleak.IgnoreCurrent()
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error { return turn(h, n, a2a.TaskStateInputRequired) }
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	<-f.agent.sent
	second := inbound("2")
	if _, err := f.submit(second); err != nil {
		t.Fatal(err)
	}
	select { // held: the agent is sent nothing while the task waits for input
	case id := <-f.agent.sent:
		t.Errorf("%s was sent although the task waits for input", id)
	case <-time.After(100 * time.Millisecond):
	}
	if err := f.close(); err != nil { // Close leaves the state as it is, for recovery
		t.Fatal(err)
	}
	if next, err := f.st.NextReply(t.Context(), key); err != nil || next.ID != second.MessageID || f.taskID() != "t1" {
		t.Errorf("queue head %+v, %v; task %q; want the second turn still queued and t1 recorded", next, err, f.taskID())
	}
	goleak.VerifyNone(t, ignore)
}

// Turns of one conversation never overlap, and run in the order they were queued (D21).
func TestTurnsRunOneAtATimeInOrder(t *testing.T) {
	f := newFixture(t)
	f.live()
	release := make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		<-release
		return turn(h, n, a2a.TaskStateCompleted)
	}
	var want []string
	for _, ts := range []string{"1", "2", "3"} {
		in := inbound(ts)
		want = append(want, in.MessageID)
		if _, err := f.submit(in); err != nil {
			t.Fatal(err)
		}
	}
	<-f.agent.sent
	select { // the second turn waits for the first
	case id := <-f.agent.sent:
		t.Errorf("%s was sent while the first turn ran", id)
	case <-time.After(100 * time.Millisecond):
	}
	for range want {
		release <- struct{}{} // each Send takes one
	}
	eventually(t, "three turns", func() bool { f.agent.mu.Lock(); defer f.agent.mu.Unlock(); return len(f.agent.msgs) == 3 })
	for i, m := range f.agent.msgs {
		if m.ID != want[i] {
			t.Errorf("turn %d = %s; want %s", i, m.ID, want[i])
		}
	}
	if f.agent.maxActive != 1 {
		t.Errorf("%d turns ran at once", f.agent.maxActive)
	}
}

// A shutdown mid-stream cancels the stream, not the task: the agent's task stays recorded.
func TestCloseLeavesAnAcceptedTurnRecorded(t *testing.T) {
	f := newFixture(t)
	ignore := goleak.IgnoreCurrent()
	f.live()
	f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-f.agent.sent
	eventually(t, "the task to be recorded", func() bool { return f.taskID() == "t1" })
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.NextReply(t.Context(), key); !errors.Is(err, store.ErrNotFound) || f.taskID() != "t1" {
		t.Errorf("queue %v, task %q; want the turn delivered and t1 recorded", err, f.taskID())
	}
	goleak.VerifyNone(t, ignore)
}

// A send that fails before the task is named leaves the turn queued, and does not spin: the
// redelivered event, waking the runner again, sends the same messageId once more.
func TestFailedSendStaysQueued(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return errors.New("connection refused")
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	in := inbound("1")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	<-f.agent.sent
	select {
	case id := <-f.agent.sent:
		t.Fatalf("%s was sent again with nothing to wake the runner", id)
	case <-time.After(100 * time.Millisecond):
	}
	if next, err := f.st.NextReply(t.Context(), key); err != nil || next.ID != in.MessageID {
		t.Fatalf("queue head %+v, %v; want the failed turn", next, err)
	}
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	if id := <-f.agent.sent; id != in.MessageID {
		t.Errorf("resent %s; want the same messageId %s", id, in.MessageID)
	}
	<-sink.finals
}

// A task that ends after its session expired clears the task but does not touch the session
// back to life (D28): the runner reads before it touches.
func TestEndedTurnDoesNotReviveAnExpiredSession(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	mid, gate := make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		mid <- struct{}{}
		<-gate
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-mid
	f.now.Store(t0.Add(2 * time.Hour).UnixNano()) // ns/b1's threadTTL is an hour
	close(gate)
	<-sink.finals
	eventually(t, "the runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
	if sess, _ := f.session(); sess.TaskID != "" || !sess.LastActivity.Equal(t0) {
		t.Errorf("session = %+v; want the task cleared and the activity still %v", sess, t0)
	}
}

// If the turn cannot be recorded, it must not be sent again: the agent has taken it, and a
// resend into its context runs it a second time (spec 8.2). The turn fails after a bounded
// retry, the runner stops, and no later wake sends it either: the head stays queued (and the
// task, if it was recorded, stays set) for reconciliation.
func TestUnrecordedTurnIsNeverResent(t *testing.T) {
	for call, wantTask := range map[string]string{"set": "", "mark": "t1"} {
		t.Run(call, func(t *testing.T) {
			f := newFixture(t)
			f.live()
			f.st.stuck[call] = errors.New("disk full")
			in := inbound("1")
			for i := range 2 { // the event, then its redelivery
				if _, err := f.submit(in); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					<-f.agent.sent
				}
				select {
				case id := <-f.agent.sent:
					t.Fatalf("%s was sent a second time", id)
				case <-time.After(150 * time.Millisecond):
				}
			}
			if next, err := f.st.NextReply(t.Context(), key); err != nil || next.ID != in.MessageID || f.taskID() != wantTask {
				t.Errorf("queue head %+v, %v; task %q; want the turn still queued and task %q", next, err, f.taskID(), wantTask)
			}
		})
	}
}

// A write that fails once is retried, and the turn goes on as if nothing happened.
func TestRecordingRetriesATransientFailure(t *testing.T) {
	f := newFixture(t)
	f.live()
	f.st.fail["mark"] = errors.New("busy")
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn to be delivered and its task cleared", func() bool {
		_, err := f.st.NextReply(t.Context(), key)
		return errors.Is(err, store.ErrNotFound) && f.taskID() == ""
	})
}

// Close returns only after the runners have: a turn that is still writing when Close returns
// would race whatever the caller does next, such as closing the store.
func TestCloseWaitsForTheRunners(t *testing.T) {
	f := newFixture(t)
	f.live()
	var finished atomic.Bool
	f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a runner that is slow to wind down
		finished.Store(true)
		return ctx.Err()
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-f.agent.sent
	if err := f.close(); err != nil || !finished.Load() {
		t.Errorf("Close = %v, runner finished %v; want it to return only after the runner did", err, finished.Load())
	}
}

// A session with a recorded task is not sent into: kagent runs one task at a time, and a turn
// resent into the context would run twice. This is the state a restart finds (a task set, the
// head undelivered) until reconciliation. It is an expected state, so it is not logged as an
// error at every wake.
func TestSessionWithAnActiveTaskIsNotSentInto(t *testing.T) {
	f := newFixture(t)
	f.live()
	var logs bytes.Buffer // the runner writes it before its turn ends, and the test reads it after
	f.m.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	f.seed("ns/b1", 0)                      // task t1 recorded
	for _, ts := range []string{"1", "2"} { // two wakes
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		select {
		case id := <-f.agent.sent:
			t.Fatalf("%s was sent although task t1 is recorded", id)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if next, err := f.st.NextReply(t.Context(), key); err != nil || next.ID != inbound("1").MessageID || f.taskID() != "t1" {
		t.Errorf("queue head %+v, %v; task %q; want the first turn still queued and t1 kept", next, err, f.taskID())
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("log = %q; a hold is expected, not an error", logs.String())
	}
}

// If the agent ends a stream without naming a task, it may still have run the turn: it is
// not sent again either.
func TestTurnWithNoNamedTaskIsNotSentAgain(t *testing.T) {
	f := newFixture(t)
	f.live()
	f.agent.script = func(context.Context, int, agent.Handler) error { return nil }
	for range 2 { // the event, then its redelivery
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
	}
	<-f.agent.sent
	select {
	case id := <-f.agent.sent:
		t.Fatalf("%s was sent again", id)
	case <-time.After(150 * time.Millisecond):
	}
}
