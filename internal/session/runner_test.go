package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// fakeAgent is an AgentAPI whose turns a test scripts: each Send announces its messageId on
// sent, then runs script with its 0-based call number.
type fakeAgent struct {
	mu     sync.Mutex
	msgs   []*a2a.Message
	sent   chan string
	script func(ctx context.Context, n int, h agent.Handler) error
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
	a.mu.Unlock()
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

// fakeSink records every event, and announces each FinalEvent on finals.
type fakeSink struct {
	mu     sync.Mutex
	events []agent.Event
	finals chan struct{}
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

// live starts real runners on Submit, with a sink, and returns the sink.
func (f *fixture) live() *fakeSink {
	sink := &fakeSink{finals: make(chan struct{}, 16)}
	f.m.cfg.Sink, f.m.wake = sink, f.m.start
	f.t.Cleanup(func() { f.m.stop(); f.m.wg.Wait() }) // Close arrives with the next layer
	return sink
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
	if next, err := f.st.NextReply(t.Context(), key); err != nil || next.ID != second.MessageID || f.taskID() != "t1" {
		t.Errorf("queue head %+v, %v; task %q; want the second turn still queued and t1 recorded", next, err, f.taskID())
	}
}
