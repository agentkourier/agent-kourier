package live_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// gatedAgent is a session.AgentAPI that takes each turn, names a task for it, and holds the task on that turn's
// gate until the test closes it. It announces each turn it has taken on taken.
type gatedAgent struct {
	mu    sync.Mutex
	sends int
	gates []chan struct{}
	taken chan string
}

func (a *gatedAgent) gate(i int) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.gates) <= i {
		a.gates = append(a.gates, make(chan struct{}))
	}
	return a.gates[i]
}

func (a *gatedAgent) Send(ctx context.Context, msg *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	i := a.sends
	a.sends++
	a.mu.Unlock()
	id := a2a.TaskID(fmt.Sprintf("t%d", i+1))
	h(agent.StateEvent{TaskID: id, ContextID: "c1", State: a2a.TaskStateWorking})
	a.taken <- msg.ID
	select {
	case <-a.gate(i):
	case <-ctx.Done():
		return ctx.Err()
	}
	h(agent.FinalEvent{Task: &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
	return nil
}

func (*gatedAgent) Follow(context.Context, a2a.TaskID, agent.Handler) error {
	return errors.New("not scripted")
}

func (*gatedAgent) GetTask(context.Context, a2a.TaskID) (*a2a.Task, error) {
	return nil, errors.New("not scripted")
}

func (*gatedAgent) CancelTask(context.Context, a2a.TaskID) (*a2a.Task, error) {
	return nil, errors.New("not scripted")
}

func (*gatedAgent) Answer(context.Context, *a2a.Message, agent.Handler) error {
	return errors.New("not scripted")
}

func (*gatedAgent) FindTask(context.Context, string, string) (*a2a.Task, []a2a.TaskID, error) {
	return nil, nil, nil
}

type oneBinding struct{ b session.Backend }

func (o oneBinding) Backend(string) (session.Backend, error) { return o.b, nil }

type noAudit struct{}

func (noAudit) AgentCall(context.Context, audit.Actor, audit.Call) error { return nil }

// idOnly resolves every user to their ID alone.
type idOnly struct{}

func (idOnly) ResolveUser(_ context.Context, id string) (dialect.User, error) {
	return dialect.User{ID: id}, nil
}

// noticeSink is the live sink with no rendering of turns, so that the thread shows nothing but what the notices do.
type noticeSink struct{ *live.Sink }

func (noticeSink) Turn(context.Context, session.TurnRef) agent.Handler { return nil }

// TestAReplyThatWaitedLosesItsHourglassWhenItIsSent runs the whole path with nothing in between: the session
// manager, the sink, the outbox, the Slack adapter and the fake Slack. A reply to a thread whose task runs gets
// the hourglass, and loses it when its turn is sent, while that turn's own task still runs. The first message, which
// waited for nothing, never has one.
func TestAReplyThatWaitedLosesItsHourglassWhenItIsSent(t *testing.T) {
	e := setup(t)
	api := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(e.fake.URL()+"/"))
	var ts [2]string
	for i := range ts {
		var err error
		if _, ts[i], err = api.PostMessage("C1", slackgo.MsgOptionText("reply", false), slackgo.MsgOptionTS(e.root)); err != nil {
			t.Fatal(err)
		}
	}
	ag := &gatedAgent{taken: make(chan string, 4)}
	m, err := session.New(session.Config{
		Store: e.store, Bindings: oneBinding{session.Backend{ThreadTTL: time.Hour, Agent: ag}}, Audit: noAudit{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Sink: noticeSink{e.sink(t, e.ob)},
		Users: idOnly{}, Questions: e.store, ThreadContext: session.NoThreadContext, OnTurnEnd: func(string, session.TurnOutcome) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	key := session.Key{Connection: "ns/conn", Channel: "C1", Thread: e.root}
	submit := func(i int) session.Result {
		t.Helper()
		res, err := m.Submit(t.Context(), session.Inbound{Key: key, Binding: "ns/b", MessageID: "slack:ns/conn:C1:" + ts[i], MessageRef: ts[i], UserID: "U1", Text: "hello", Create: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	reacted := func(i int) bool {
		t.Helper()
		if err := e.ob.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
		return len(e.fake.Reactions("C1", ts[i])) > 0
	}

	if res := submit(0); res.Queued {
		t.Fatalf("the first message = %+v; want it not queued", res)
	}
	<-ag.taken // its task runs
	if res := submit(1); !res.Queued {
		t.Fatalf("the reply = %+v; want it queued behind the running task", res)
	}
	if !reacted(1) {
		t.Fatalf("Reactions on the waiting reply = %+v; want the hourglass", e.fake.Reactions("C1", ts[1]))
	}
	if reacted(0) {
		t.Fatalf("Reactions on the first message = %+v; want none: it waited for nothing", e.fake.Reactions("C1", ts[0]))
	}

	close(ag.gate(0)) // the first task ends, and the reply is sent; its own task is held on the second gate
	<-ag.taken
	eventually(t, "the hourglass to come off the reply that is now sent", func() bool { return !reacted(1) })
	close(ag.gate(1))
}
