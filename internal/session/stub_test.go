package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// countingAgent counts the Sends and lookups that reach the stub, through the real client.
type countingAgent struct {
	*agent.Agent
	sends, finds atomic.Int32
}

func (a *countingAgent) Send(ctx context.Context, msg *a2a.Message, h agent.Handler) error {
	a.sends.Add(1)
	return a.Agent.Send(ctx, msg, h)
}

func (a *countingAgent) FindTask(ctx context.Context, contextID, messageID string) (*a2a.Task, []a2a.TaskID, error) {
	a.finds.Add(1)
	return a.Agent.FindTask(ctx, contextID, messageID)
}

// stubEnv is the manager's runner against the stub agent kagent/test, through the kagent
// dialect and the real client: the wire behavior of spec 8.2.
type stubEnv struct {
	*fixture
	sink  *fakeSink
	srv   *stubagent.Server
	stub  *stubagent.Agent
	agent *countingAgent
}

func newStubEnv(t *testing.T, opts ...stubagent.Option) *stubEnv {
	t.Helper()
	srv := stubagent.New(opts...)
	t.Cleanup(srv.Close)
	pool := agent.NewPool(kagentv1.New(0), agent.Config{})
	t.Cleanup(pool.Close)
	ag := &countingAgent{Agent: pool.Agent(agent.Target{
		Binding:     "ns/b1",
		Backend:     &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: srv.URL()}},
		Agent:       v1alpha1.AgentRef{Namespace: "kagent", Name: "test"},
		Credentials: func(context.Context) (dialect.Credentials, error) { return dialect.Credentials{UserID: "courier"}, nil },
	})}
	f := newFixture(t)
	f.real = ag
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = 5*time.Millisecond, 20*time.Millisecond
	return &stubEnv{fixture: f, sink: f.live(), srv: srv, stub: srv.Agent("kagent", "test"), agent: ag}
}

// finalTask waits for the next n FinalEvents of the sink and returns the last of them: the
// task a turn ended, or paused, in.
func (s *fakeSink) finalTask(t *testing.T, n int) *a2a.Task {
	t.Helper()
	for range n {
		select {
		case <-s.finals:
		case <-time.After(10 * time.Second):
			t.Fatal("no final event")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken += n
	var tasks []*a2a.Task
	for _, e := range s.events {
		if fe, ok := e.(agent.FinalEvent); ok {
			tasks = append(tasks, fe.Task)
		}
	}
	return tasks[s.taken-1]
}

// says reports whether the task's output has an artifact whose text is text.
func says(task *a2a.Task, text string) bool {
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			if p.Text() == text {
				return true
			}
		}
	}
	return false
}

// S4: a reply sent while a task runs is queued, and delivered as the next turn of the same
// context after it, under its own messageId (spec 8.2, D21).
func TestStubQueuedReplyIsDeliveredAfterTheTaskEnds(t *testing.T) {
	e := newStubEnv(t)
	e.stub.Next(stubagent.Hold("first"), stubagent.Text("first answer"))
	e.stub.Next(stubagent.Text("second answer"))
	if _, err := e.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first task to run", func() bool { return e.taskID() != "" })
	res, err := e.submit(inbound("2"))
	if err != nil || !res.Queued {
		t.Fatalf("reply = %+v, %v; want it queued behind the running task", res, err)
	}
	e.srv.Release("first")
	e.sink.finalTask(t, 1)
	second := e.sink.finalTask(t, 1)
	if second.Status.State != a2a.TaskStateCompleted || !says(second, "second answer") {
		t.Errorf("second turn = %+v; want it completed with its own answer", second)
	}
	sess, _ := e.session()
	for _, ts := range []string{"1", "2"} {
		task, holders, err := e.agent.Agent.FindTask(t.Context(), sess.ContextID, inbound(ts).MessageID)
		if err != nil || task == nil || len(holders) != 1 {
			t.Errorf("turn %s: task %v, holders %v, %v; want one task in the session's context", ts, task, holders, err)
		}
	}
}

// pausedOn returns the interaction a paused turn asks for. The request reaches a client only if
// its send carried the HITL extension header (spec 6.3), so a parsed interaction proves it.
func pausedOn(t *testing.T, task *a2a.Task) *dialect.Interaction {
	t.Helper()
	in, err := (&kagentv1.Dialect{}).ParseInteraction(*task)
	if err != nil || in == nil || in.Kind != dialect.AskUser || len(in.Questions) != 1 || in.Questions[0].Text != "Which database?" {
		t.Fatalf("interaction = %+v, %v; want the structured ask-user request, which only a send with the HITL header gets", in, err)
	}
	return in
}

var askDatabase = stubagent.Ask([]stubagent.Question{{Question: "Which database?", Choices: []string{"PostgreSQL", "MySQL"}}}, nil)

func TestStubFirstSendCarriesTheHITLHeader(t *testing.T) {
	e := newStubEnv(t)
	e.stub.Next(askDatabase)
	if _, err := e.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	task := e.sink.finalTask(t, 1)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s; want the pause", task.Status.State)
	}
	pausedOn(t, task)
}

// A session busy with a task courier never saw answers -32004. The turn stays queued and is
// resent, after a lookup each time, until the session is free; the resend carries the HITL
// header too. Nothing is dropped, and the turn runs once.
func TestStubBusySessionIsRetriedUntilItIsFree(t *testing.T) {
	e := newStubEnv(t)
	e.stub.Next(stubagent.Text("one"))
	if _, err := e.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	e.sink.finalTask(t, 1)
	eventually(t, "the first turn to end", func() bool { return e.taskID() == "" })
	sess, _ := e.session()
	// Someone else starts a task in the session's context and holds it.
	e.stub.Next(stubagent.Hold("other"), stubagent.Text("other's answer"))
	other := &a2a.Message{ID: "other-1", ContextID: sess.ContextID, Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart("hi")}}
	go func() { _ = e.agent.Agent.Send(t.Context(), other, nil) }()
	eventually(t, "the other task to run", func() bool {
		task, _, err := e.agent.Agent.FindTask(t.Context(), sess.ContextID, other.ID)
		return err == nil && task != nil
	})
	e.stub.Next(askDatabase)
	sendsBefore := e.agent.sends.Load()
	if _, err := e.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn to be rejected and resent", func() bool { return e.agent.sends.Load() >= sendsBefore+3 })
	e.srv.Release("other")
	task := e.sink.finalTask(t, 1)
	pausedOn(t, task)
	if sends, finds := e.agent.sends.Load()-sendsBefore, e.agent.finds.Load(); finds != sends-1 {
		t.Errorf("%d sends of turn 2 and %d lookups; want a lookup before every resend, and none before the first send", sends, finds)
	}
	if _, holders, _ := e.agent.Agent.FindTask(t.Context(), sess.ContextID, inbound("2").MessageID); len(holders) != 1 {
		t.Errorf("turn 2 ran %d times; want once", len(holders))
	}
}

// The front door refuses the Binding's credentials, here as a 401 at the card fetch: the turn
// fails at once, with no retry, the identity Source's Rejected is called (which writes the
// only audit row), and the thread is told (D34, spec 8.2).
func TestStubRefusedCredentialsFailTheTurn(t *testing.T) {
	e := newStubEnv(t, stubagent.WithRequiredAuth()) // the fixture's credentials carry no token
	if _, err := e.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failure notice", func() bool { return len(e.sink.noticesOf(NoticeFailed)) == 1 })
	e.retired(t) // a retry would keep the runner alive
	e.amu.Lock()
	defer e.amu.Unlock()
	if len(e.rejected) != 1 || !errors.Is(e.rejected[0].Err, agent.ErrCredentialsRejected) || len(e.audits) != 0 {
		t.Errorf("Rejected calls %+v, audits %+v; want one Rejected call and no other audit", e.rejected, e.audits)
	}
	if sends := e.agent.sends.Load(); sends != 1 {
		t.Errorf("%d sends; want one: a refused credential is not retried", sends)
	}
}

// twoTurns queues turn 1 and, behind it, turn 2 (which the stub answers "second").
func (e *stubEnv) twoTurns(t *testing.T) {
	t.Helper()
	e.stub.Next(stubagent.Text("second"))
	for _, ts := range []string{"1", "2"} {
		if _, err := e.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
}

// The stop control cancels a task that runs, at the stub, and the queue goes on with the reply.
func TestStubStopCancelsTheRunningTask(t *testing.T) {
	e := newStubEnv(t)
	e.stub.Next(stubagent.Hold("never"), stubagent.Text("never said"))
	if _, err := e.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task to run", func() bool { return e.taskID() != "" })
	task1 := a2a.TaskID(e.taskID())
	e.stub.Next(stubagent.Text("second"))
	if _, err := e.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	if did, err := e.m.Stop(t.Context(), key, task1, "U9"); err != nil || !did {
		t.Fatalf("Stop = %v, %v; want it to stop the task", did, err)
	}
	if first := e.sink.finalTask(t, 1); first.Status.State != a2a.TaskStateCanceled {
		t.Errorf("first turn ended %s; want cancelled", first.Status.State)
	}
	if second := e.sink.finalTask(t, 1); second.Status.State != a2a.TaskStateCompleted || !says(second, "second") {
		t.Errorf("second turn = %+v; want it completed", second)
	}
	e.amu.Lock()
	defer e.amu.Unlock()
	if len(e.audits) != 1 || e.audits[0].Op != audit.OpCancel || e.actors[0].User != "U9" {
		t.Errorf("audits %+v by %+v; want the cancel, by U9", e.audits, e.actors)
	}
}

// S3, from the session's side: the agent asks a question, the dialect builds the answer from a
// choice, and Answer sends it; the task then completes, and the reply held behind the pause goes.
func TestStubAskThenAnswer(t *testing.T) {
	e := newStubEnv(t)
	e.stub.Next(stubagent.Ask([]stubagent.Question{{Question: "Which database?", Choices: []string{"PostgreSQL", "MySQL"}}},
		func(r stubagent.Response) []stubagent.Step {
			return []stubagent.Step{stubagent.Text("chose " + r.Answers[0][0])}
		}))
	e.twoTurns(t)
	in := pausedOn(t, e.sink.finalTask(t, 1))
	msg, err := (&kagentv1.Dialect{}).BuildResponse(*in, dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"PostgreSQL"}}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the runner to park", func() bool { return e.m.Answer(t.Context(), key, &msg) == nil })
	if answered := e.sink.finalTask(t, 1); answered.Status.State != a2a.TaskStateCompleted || !says(answered, "chose PostgreSQL") {
		t.Errorf("answered turn = %+v; want it completed with the chosen database", answered)
	}
	if second := e.sink.finalTask(t, 1); !says(second, "second") {
		t.Errorf("second turn = %+v; want the held reply, sent after the answer", second)
	}
}

// R17, end to end: an answer kagent cannot correlate wedges the task in WORKING. Agent.Answer
// cancels it, the thread is told, the call is audited, and the held reply goes out.
func TestStubBadAnswerWedgesAndIsCancelled(t *testing.T) {
	e := newStubEnv(t)
	e.stub.Next(askDatabase)
	e.twoTurns(t)
	task := e.sink.finalTask(t, 1)
	in := pausedOn(t, task)
	msg, err := (&kagentv1.Dialect{}).BuildResponse(*in, dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"MySQL"}}})
	if err != nil {
		t.Fatal(err)
	}
	msg.Metadata[kagentv1.HITLExtension].(map[string]any)["id"] = "not-the-request" // the wedge of WP-02
	eventually(t, "the runner to park", func() bool { return e.m.Answer(t.Context(), key, &msg) == nil })
	if second := e.sink.finalTask(t, 1); !says(second, "second") {
		t.Fatalf("second turn = %+v; want the held reply, sent after the wedge was cancelled", second)
	}
	if got := e.sink.noticesOf(NoticeAnswerRejected); len(got) != 1 {
		t.Errorf("answer-rejected notices = %+v; want one", got)
	}
	if wedged, err := e.agent.GetTask(t.Context(), task.ID); err != nil || wedged.Status.State != a2a.TaskStateCanceled {
		t.Errorf("the wedged task = %+v, %v; want it cancelled at the stub", wedged, err)
	}
	e.amu.Lock()
	defer e.amu.Unlock()
	if len(e.audits) != 2 || e.audits[0].Op != audit.OpAnswer || e.audits[1].Op != audit.OpCancel {
		t.Errorf("audits = %+v; want the rejected answer, then the cancel Agent.Answer made", e.audits)
	}
}
