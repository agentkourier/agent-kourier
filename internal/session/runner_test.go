package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
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
	hold              chan struct{} // if set, the first Send waits for it to close, before it runs script
	// find and follow answer FindTask and Follow; finds and follows record their calls.
	find      func(contextID, messageID string) (*a2a.Task, []a2a.TaskID, error)
	follow    func(id a2a.TaskID, h agent.Handler) error
	finds     []string
	follows   []a2a.TaskID
	get       func(id a2a.TaskID) (*a2a.Task, error)
	cancels   []a2a.TaskID
	cancelErr func(attempt int) error // the error of the n-th CancelTask (1-based), if any
	onCancel  func(id a2a.TaskID)     // runs inside CancelTask, before it returns
	answer    func(msg *a2a.Message, h agent.Handler) error
	answers   []*a2a.Message
	seq       []string // the calls in order: send, find, follow
}

func (a *fakeAgent) FindTask(_ context.Context, contextID, messageID string) (*a2a.Task, []a2a.TaskID, error) {
	a.mu.Lock()
	a.finds = append(a.finds, contextID+" "+messageID)
	a.seq = append(a.seq, "find")
	find := a.find
	a.mu.Unlock()
	if find == nil {
		return nil, nil, nil
	}
	return find(contextID, messageID)
}

func (a *fakeAgent) GetTask(_ context.Context, id a2a.TaskID) (*a2a.Task, error) {
	a.mu.Lock()
	get := a.get
	a.mu.Unlock()
	return get(id)
}

func (a *fakeAgent) CancelTask(_ context.Context, id a2a.TaskID) (*a2a.Task, error) {
	a.mu.Lock()
	a.cancels = append(a.cancels, id)
	hook, cerr, attempt := a.onCancel, a.cancelErr, len(a.cancels)
	a.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if cerr != nil {
		if err := cerr(attempt); err != nil {
			return nil, err
		}
	}
	return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

func (a *fakeAgent) Answer(_ context.Context, msg *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	a.answers = append(a.answers, msg)
	answer := a.answer
	a.mu.Unlock()
	return answer(msg, h)
}

func (a *fakeAgent) Follow(_ context.Context, id a2a.TaskID, h agent.Handler) error {
	a.mu.Lock()
	a.follows = append(a.follows, id)
	a.seq = append(a.seq, "follow")
	a.mu.Unlock()
	return a.follow(id, h)
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sent: make(chan string, 16), get: func(a2a.TaskID) (*a2a.Task, error) { return nil, errors.New("GetTask is not scripted") }, script: func(_ context.Context, n int, h agent.Handler) error {
		return turn(h, n, a2a.TaskStateCompleted)
	}}
}

func (a *fakeAgent) Send(ctx context.Context, msg *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	n := len(a.msgs)
	a.msgs = append(a.msgs, msg)
	a.seq = append(a.seq, "send")
	a.active++
	a.maxActive = max(a.maxActive, a.active)
	hold := a.hold
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.active--; a.mu.Unlock() }()
	a.sent <- msg.ID
	if n == 0 && hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	mu       sync.Mutex
	events   []agent.Event
	notices  []Notice
	refs     []TurnRef
	ctxs     []context.Context // the context each Turn was given
	taken    int               // FinalEvents finalTask has returned
	finals   chan struct{}
	onFinal  func(*a2a.Task) // runs inside the handler, before finals is signalled
	onNotice func(Notice)    // runs inside Notice, before it is recorded
}

func (s *fakeSink) Notice(_ context.Context, _ Key, n Notice) {
	if s.onNotice != nil {
		s.onNotice(n)
	}
	s.mu.Lock()
	s.notices = append(s.notices, n)
	s.mu.Unlock()
}

func (s *fakeSink) Turn(ctx context.Context, ref TurnRef) agent.Handler {
	s.mu.Lock()
	s.refs, s.ctxs = append(s.refs, ref), append(s.ctxs, ctx)
	s.mu.Unlock()
	return func(e agent.Event) {
		s.mu.Lock()
		s.events = append(s.events, e)
		s.mu.Unlock()
		if fe, ok := e.(agent.FinalEvent); ok {
			if s.onFinal != nil {
				s.onFinal(fe.Task)
			}
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
	// Not t.Context(): that is already cancelled when a Cleanup runs, and Close would not wait.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return f.m.Close(ctx)
}

// eventually polls cond until it holds. The deadline is only how long a condition that will never hold is waited
// for before the test fails, and is long: a loaded machine can starve the goroutine that makes it hold.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(time.Millisecond) {
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
	f.attr, f.platform = (&kagentv1.Dialect{}).Attribute, dialect.Platform{ID: "slack", Name: "Slack"}
	var logs bytes.Buffer // written by the runner, read once it has retired
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
		// The Backend's platform is on the actor whether or not the platform resolved the user.
		actor, _ := msg.Metadata[kagentv1.ActorMetadataKey].(map[string]any)
		if tc.user != "" && actor["platform"] != "slack" || tc.user == "" && actor != nil {
			t.Errorf("turn %d actor = %v; want platform slack for a person's turn and no actor for an alert's", i, actor)
		}
	}
	f.retired(t) // the runner logs the end of a turn after its final event
	if got := logs.String(); !strings.Contains(got, "U2") || !strings.Contains(got, "no such user") {
		t.Errorf("log = %q; want the failed resolution of U2 and its cause", got)
	}
}

// A turn's sink is given a context of its own, which ends when the send returns, so that a turn
// that ends with no FinalEvent frees what the renderer started for it. It is told who asked (the
// system user for an alert) and the Binding's output mode.
func TestSinkTurnGetsItsOwnContextAndWhatTheRendererNeeds(t *testing.T) {
	f := newFixture(t)
	f.output = v1alpha1.OutputFinal
	sink := f.live()
	for _, user := range []string{"U1", ""} {
		in := inbound(user + "x")
		in.UserID = user
		if _, err := f.submit(in); err != nil {
			t.Fatal(err)
		}
		<-sink.finals
	}
	// Copy under the lock and let it go: the final event is announced from inside the runner's handler, which goes on
	// to tell the sink of the turn's end (Notice takes this lock), and it ends the context only after that.
	sink.mu.Lock()
	refs, ctxs := slices.Clone(sink.refs), slices.Clone(sink.ctxs)
	sink.mu.Unlock()
	if len(refs) != 2 || refs[0].UserID != "U1" || refs[1].UserID != SystemUser || refs[0].Output != v1alpha1.OutputFinal || refs[0].Recovered || refs[0].Binding != "ns/b1" || refs[1].Binding != "ns/b1" {
		t.Fatalf("turn refs = %+v; want the asking user, the system user for an alert, the output mode and the Binding of the thread", refs)
	}
	for i, ctx := range ctxs {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("the context of turn %d is still open after its send returned", i)
		}
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

// counts returns how many sends, lookups and follows the agent has had.
func (a *fakeAgent) counts() (sends, finds, follows int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.msgs), len(a.finds), len(a.follows)
}

// If the turn cannot be recorded, the runner stops instead of resending it in a loop, and
// what a later wake does is reconciliation (spec 8.2). With a context, the lookup finds the
// turn the agent took, which is followed, never sent again. A first turn has no context to
// look in, and its resend, with the same messageId and none, is deduplicated by the agent.
func TestUnrecordedTurnIsReconciledNotResent(t *testing.T) {
	for _, call := range []string{"mark", "set"} { // "mark" leaves the context recorded, "set" does not
		t.Run(call, func(t *testing.T) {
			f := newFixture(t)
			f.live()
			f.st.stuck[call] = errors.New("disk full")
			f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
				return &a2a.Task{ID: "t1", ContextID: "c1"}, []a2a.TaskID{"t1"}, nil
			}
			f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
			in := inbound("1")
			if _, err := f.submit(in); err != nil {
				t.Fatal(err)
			}
			<-f.agent.sent
			select { // with nothing to wake it, the runner does not send again
			case id := <-f.agent.sent:
				t.Fatalf("%s was sent a second time", id)
			case <-time.After(150 * time.Millisecond):
			}
			if _, err := f.submit(in); err != nil { // the redelivery
				t.Fatal(err)
			}
			if call == "mark" {
				eventually(t, "the turn to be followed", func() bool { _, _, fo := f.agent.counts(); return fo == 1 })
				if sends, _, _ := f.agent.counts(); sends != 1 {
					t.Errorf("%d sends; want the turn followed through the lookup, not sent again", sends)
				}
				return
			}
			eventually(t, "the turn to be sent again", func() bool { s, _, _ := f.agent.counts(); return s == 2 })
			f.agent.mu.Lock()
			defer f.agent.mu.Unlock()
			if m := f.agent.msgs[1]; m.ID != in.MessageID || m.ContextID != "" || len(f.agent.finds) != 0 {
				t.Errorf("resent %s in context %q after %d lookups; want the same messageId, no context, no lookup", m.ID, m.ContextID, len(f.agent.finds))
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

// If the agent ends a stream without naming a task, it may still have run the turn: the
// next wake looks it up, and follows what it finds, instead of sending it into the context.
func TestTurnWithNoNamedTaskIsLookedUpNotResent(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return nil // turn 2: the stream ends clean, naming nothing
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return &a2a.Task{ID: "t9", ContextID: "c1"}, []a2a.TaskID{"t9"}, nil
	}
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 8, a2a.TaskStateCompleted) }
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	eventually(t, "turn 1 to end", func() bool { return f.taskID() == "" })
	for range 2 { // the event, then its redelivery
		if _, err := f.submit(inbound("2")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	eventually(t, "the turn to be followed", func() bool { _, _, fo := f.agent.counts(); return fo >= 1 })
	if sends, _, _ := f.agent.counts(); sends != 2 {
		t.Errorf("%d sends; want turn 1, and turn 2 once", sends)
	}
	eventually(t, "the hold to be lifted", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.unrecorded) == 0 })
}

// A turn queued for a session whose Binding then left the config is not retried: the thread is
// told, and the turn leaves the queue.
func TestQueuedTurnOfARemovedBindingIsDropped(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.wake = func(Key) {} // queue the turn first, run it after the Binding is gone
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/gone", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	f.m.start(key)
	eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeBindingGone)) == 1 })
	eventually(t, "the turn to leave the queue", func() bool { _, err := f.st.NextReply(t.Context(), key); return errors.Is(err, store.ErrNotFound) })
	if sends, _, _ := f.agent.counts(); sends != 0 {
		t.Errorf("%d sends; want none", sends)
	}
}

// cancelled returns the tasks the agent was asked to cancel.
func (f *fixture) cancelled() []a2a.TaskID {
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return append([]a2a.TaskID(nil), f.agent.cancels...)
}

// A Binding lookup that fails for another reason than a removed Binding must never drop a live
// turn: it stays queued, with no notice.
func TestTransientBindingErrorLeavesTheTurnQueued(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.wake = func(Key) {}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	f.amu.Lock()
	f.bindingErr = errors.New("config reload in progress")
	f.amu.Unlock()
	f.m.start(key)
	f.retired(t) // the runner gives up on this wake, leaving the turn queued
	if next, err := f.st.NextReply(t.Context(), key); err != nil || next.ID != inbound("1").MessageID || len(sink.noticesOf(NoticeBindingGone)) != 0 {
		t.Errorf("queue head %+v, %v; want the turn still queued and no notice", next, err)
	}
}

// countWaits makes every wait for a retry endless, and counts them: a runner that has stopped
// instead of retrying shows as zero waits once it has retired.
func (f *fixture) countWaits() *atomic.Int32 {
	var n atomic.Int32
	f.m.cfg.After = func(time.Duration) <-chan time.Time { n.Add(1); return nil }
	return &n
}

// retired waits for the runner of the conversation to stop.
func (f *fixture) retired(t *testing.T) {
	t.Helper()
	eventually(t, "the runner to retire", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.convos) == 0 })
}

// fresh is bounded by the turns still queued: every way a turn leaves the queue forgets it. A
// turn the agent took, one failed for good, and one dropped for a removed Binding all do. (A held,
// unrecorded turn keeps its entry while its row is queued, which is what lets it be looked up.)
func TestFreshHoldsNoTurnThatLeftTheQueue(t *testing.T) {
	fresh := func(f *fixture) int { f.m.mu.Lock(); defer f.m.mu.Unlock(); return len(f.m.fresh) }
	t.Run("a turn the agent took", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		eventually(t, "the turn to be forgotten", func() bool { return fresh(f) == 0 })
	})
	t.Run("a turn failed for good", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		f.agent.script = func(context.Context, int, agent.Handler) error { return fmt.Errorf("send: %w", agent.ErrForbidden) }
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the failure notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
		eventually(t, "the turn to be forgotten", func() bool { return fresh(f) == 0 })
	})
	t.Run("a turn dropped for a removed Binding", func(t *testing.T) {
		f := newFixture(t)
		sink := f.live()
		f.m.wake = func(Key) {}
		for _, ts := range []string{"1", "2", "3"} {
			if _, err := f.submit(inbound(ts)); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/gone", LastActivity: t0}); err != nil {
			t.Fatal(err)
		}
		f.m.start(key)
		eventually(t, "the turns to be dropped", func() bool { return len(sink.noticesOf(NoticeBindingGone)) == 3 })
		eventually(t, "the turns to be forgotten", func() bool { return fresh(f) == 0 })
	})
}
