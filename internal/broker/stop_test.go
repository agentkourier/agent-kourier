package broker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

type stopTestAgent struct {
	mu        sync.Mutex
	cancelErr error
	cancelled []a2a.TaskID
	sent      []string
}

func (a *stopTestAgent) Send(_ context.Context, msg *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	a.sent = append(a.sent, msg.ID)
	a.mu.Unlock()
	h(agent.StateEvent{TaskID: "t-next", ContextID: "ctx-1", State: a2a.TaskStateWorking})
	h(agent.FinalEvent{Task: &a2a.Task{ID: "t-next", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
	return nil
}

func (*stopTestAgent) Follow(context.Context, a2a.TaskID, agent.Handler) error {
	return errors.New("not scripted")
}

func (*stopTestAgent) GetTask(context.Context, a2a.TaskID) (*a2a.Task, error) {
	return nil, errors.New("not scripted")
}

func (a *stopTestAgent) CancelTask(_ context.Context, id a2a.TaskID) (*a2a.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelled = append(a.cancelled, id)
	if a.cancelErr != nil {
		return nil, a.cancelErr
	}
	return &a2a.Task{ID: id, ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

func (*stopTestAgent) Answer(context.Context, *a2a.Message, agent.Handler) error {
	return errors.New("not scripted")
}

func (*stopTestAgent) FindTask(context.Context, string, string) (*a2a.Task, []a2a.TaskID, error) {
	return nil, nil, nil
}

type stopTestSink struct {
	mu      sync.Mutex
	notices []session.Notice
}

func (*stopTestSink) Turn(context.Context, session.TurnRef) agent.Handler { return nil }

func (s *stopTestSink) Notice(_ context.Context, _ session.Key, n session.Notice) {
	s.mu.Lock()
	s.notices = append(s.notices, n)
	s.mu.Unlock()
}

func (s *stopTestSink) has(kind session.NoticeKind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.notices, func(n session.Notice) bool { return n.Kind == kind })
}

type stopTestBindings struct{ b session.Backend }

func (s stopTestBindings) Backend(string) (session.Backend, error) { return s.b, nil }

type stopTestUsers struct{}

func (stopTestUsers) ResolveUser(_ context.Context, _, id string) (dialect.User, error) {
	return dialect.User{ID: id}, nil
}

type stopTestAudit struct{}

func (stopTestAudit) AgentCall(context.Context, audit.Actor, audit.Call) error { return nil }

type stopTestNoopPresser struct{ err error }

func (p stopTestNoopPresser) Press(context.Context, chat.Interaction) error { return p.err }
func (p stopTestNoopPresser) DecidePress(context.Context, chat.Interaction) (chat.PressOutcome, error) {
	return chat.PressOutcome{}, p.err
}

// Pressing Stop on a stranded task routes through pressHandler -> pressRouter -> StopHandler -> lateSession ->
// session.Manager.Stop, cancels the task, emits NoticeStopped (or NoticeStopUnverified when the cancel answer is too
// large to read), clears the recorded task, and unblocks queued replies.
func TestStopControlEndToEndCancelsStrandedTaskAndUnblocksQueue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelErr  error
		wantNotice session.NoticeKind
	}{
		{"normal cancel", nil, session.NoticeStopped},
		{"unverified oversized cancel", fmt.Errorf("cancel task: %w", dialect.ErrResponseTooLarge), session.NoticeStopUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sqlitetest.Open(t)
			ag := &stopTestAgent{cancelErr: tc.cancelErr}
			sink := &stopTestSink{}
			now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

			mgr, err := session.New(session.Config{
				Store:         st,
				Bindings:      stopTestBindings{b: session.Backend{ThreadTTL: time.Hour, Agent: ag}},
				Audit:         stopTestAudit{},
				Users:         stopTestUsers{},
				Sink:          sink,
				Questions:     st,
				TextReply:     session.NoTextReply,
				ThreadContext: session.NoThreadContext,
				OnTurnEnd:     func(string, session.TurnOutcome) {},
				Now:           func() time.Time { return now },
				Logger:        testutil.DiscardLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = mgr.Close(ctx)
			})

			key := store.ConversationKey{Connection: "ns/slack", Channel: "C1", Thread: "100.1"}
			if err := st.UpsertSession(t.Context(), store.Session{
				Key: key, Binding: "ns/sre", ContextID: "ctx-1", TaskID: "t-stranded",
				Origin: store.Origin{Source: "mention"}, LastActivity: now,
			}); err != nil {
				t.Fatal(err)
			}
			// Queue a reply behind the stranded task.
			res, err := mgr.Submit(t.Context(), session.Inbound{
				Key: key, Binding: "ns/sre", MessageID: "msg-queued", MessageRef: "100.5", UserID: "U1", Text: "follow up",
			})
			if err != nil || !res.Queued {
				t.Fatalf("Submit = %+v, %v; want Queued=true", res, err)
			}

			fake := chattest.NewFake()
			fake.AddUser(chat.User{ID: "U1"}, false)
			fake.SetMembers("C1", "U1")
			late := &lateSession{}
			late.bind(mgr)
			var worker *interact.Worker
			stopH, err := interact.NewStopHandler(interact.StopConfig{
				Store:       st,
				Sessions:    st,
				Authorizers: chattest.Same[interact.Authorizer](fake),
				Ephemerals:  chattest.Same[interact.Ephemeral](fake),
				Stopper:     late,
				Notify:      func(p store.Press) { worker.Submit(p) },
				Logger:      testutil.DiscardLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			worker, err = interact.NewWorker(interact.WorkerConfig{
				Processor: pressRouter{stops: stopH},
				Presses:   st,
				Logger:    testutil.DiscardLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			wctx, wcancel := context.WithCancel(t.Context())
			wdone := make(chan struct{})
			go func() { defer close(wdone); _ = worker.Run(wctx) }()
			t.Cleanup(func() { wcancel(); <-wdone })

			ph := interactionsHandler(
				stopTestNoopPresser{err: interact.ErrNotInteraction},
				stopTestNoopPresser{err: feedback.ErrNotFeedback},
				stopH,
				testutil.DiscardLogger(),
			)
			if err := ph.Interaction(t.Context(), chat.Interaction{
				User:     "U1",
				At:       chat.Ref{Conn: key.Connection, Channel: key.Channel, Thread: key.Thread, ID: "100.3"},
				PromptID: render.StopValue{Thread: key.Thread, TaskID: "t-stranded"}.String(),
				Delivery: "deliv-1",
			}); err != nil {
				t.Fatal(err)
			}

			testutil.Eventually(t, "notice emitted and queued reply delivered", func() bool {
				ag.mu.Lock()
				sent := slices.Contains(ag.sent, "msg-queued")
				cancelled := slices.Contains(ag.cancelled, a2a.TaskID("t-stranded"))
				ag.mu.Unlock()
				return sent && cancelled && sink.has(tc.wantNotice)
			})
		})
	}
}
