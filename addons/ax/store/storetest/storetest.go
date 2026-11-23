// Package storetest is the conformance suite of what the agent's store adds to the kit's task state, which the kit's
// own suite (addons/kit/store/storetest) covers: ADK's sessions on the store's database, deleted with an idle context,
// and kept across a reopen. The agent's SQLite and Postgres stores both run it, so that the agent can rely on the same
// behaviour from either.
package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

// Harness opens a backend's stores.
type Harness struct {
	// New returns a fresh, empty location (a file path, a schema), removed when the test ends.
	New func(t *testing.T) string
	// Open opens the store at location with the clock now. Opening a location again, after Close, finds what the
	// last store wrote there. The harness closes the store when the test ends; a test may close it first.
	Open func(t *testing.T, location string, now func() time.Time) *store.Store
}

// Clock is a settable time.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock on by d.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

// Run runs the suite against h.
func Run(t *testing.T, h Harness) {
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"DeleteIdleContexts", testDeleteIdleContexts},
		{"TouchContext", testTouchContext},
		{"ADKSessions", testADKSessions},
		{"Reopen", testReopen},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

func open(t *testing.T, h Harness) (*store.Store, *Clock) {
	t.Helper()
	clk := NewClock()
	return h.Open(t, h.New(t), clk.Now), clk
}

// AsCaller is a context whose call runs as the authenticated caller name.
func AsCaller(name string) context.Context {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx
}

// kourier is the caller most tests run as.
var kourier = AsCaller("agent-kourier")

// task is a task with a user message, as the agent's server creates one.
func task(id, contextID string, state a2a.TaskState) *a2a.Task {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello "+id))
	msg.ID, msg.TaskID, msg.ContextID = "msg-"+id, a2a.TaskID(id), contextID
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: contextID, Status: a2a.TaskStatus{State: state}, History: []*a2a.Message{msg}}
}

func mustCreate(t *testing.T, s *store.Store, tk *a2a.Task) {
	t.Helper()
	if _, err := s.Create(kourier, tk); err != nil {
		t.Fatalf("Create %s: %v", tk.ID, err)
	}
}

// session starts an ADK session for the context, as the agent's runner does, with one event.
func adkSession(t *testing.T, s *store.Store, user, contextID string) {
	t.Helper()
	created, err := s.Sessions().Create(context.Background(), &session.CreateRequest{AppName: store.AppName, UserID: user, SessionID: contextID})
	if err != nil {
		t.Fatalf("create session %s: %v", contextID, err)
	}
	ev := session.NewEvent(context.Background(), "invocation-"+contextID)
	ev.Author = "user"
	ev.Content = genai.NewContentFromText("hello", genai.RoleUser)
	if err := s.Sessions().AppendEvent(context.Background(), created.Session, ev); err != nil {
		t.Fatalf("append an event to %s: %v", contextID, err)
	}
}

func hasSession(t *testing.T, s *store.Store, user, contextID string) bool {
	t.Helper()
	_, err := s.Sessions().Get(context.Background(), &session.GetRequest{AppName: store.AppName, UserID: user, SessionID: contextID})
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get session %s: %v", contextID, err)
	}
	return err == nil
}

func testDeleteIdleContexts(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	other := AsCaller("someone-else")
	// Idle: a settled task and a question nobody answered.
	mustCreate(t, s, task("idle-done", "idle", a2a.TaskStateCompleted))
	mustCreate(t, s, task("idle-paused", "idle", a2a.TaskStateInputRequired))
	adkSession(t, s, "agent-kourier", "idle")
	// Idle by its last update, but a turn is running in it.
	mustCreate(t, s, task("busy-working", "busy", a2a.TaskStateWorking))
	adkSession(t, s, "agent-kourier", "busy")
	// Another caller's context of the same ID, active later.
	adkSession(t, s, "someone-else", "idle")
	clk.Advance(time.Hour)
	if _, err := s.Create(other, task("other-done", "idle", a2a.TaskStateCompleted)); err != nil {
		t.Fatal(err)
	}
	// Active: its first task is old, its last update is not.
	mustCreate(t, s, task("active-done", "active", a2a.TaskStateCompleted))
	adkSession(t, s, "agent-kourier", "active")
	clk.Advance(time.Hour)
	active, err := s.Get(kourier, "active-done")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(kourier, &taskstore.UpdateRequest{Task: active.Task, PrevVersion: active.Version}); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-90*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("DeleteIdleContexts = %d, %v; want 1", n, err)
	}
	if hasSession(t, s, "agent-kourier", "idle") {
		t.Error("the idle context's session is kept")
	}
	for _, id := range []string{"idle-done", "idle-paused"} {
		if _, err := s.Get(kourier, a2a.TaskID(id)); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("%s: %v, want deleted", id, err)
		}
		if got, err := s.MessageTask(kourier, "msg-"+id); err != nil || got != nil {
			t.Errorf("message of %s: %+v, %v; want deleted", id, got, err)
		}
	}
	for user, ctxID := range map[string]string{"agent-kourier": "busy", "someone-else": "idle"} {
		if !hasSession(t, s, user, ctxID) {
			t.Errorf("%s's session of %s is gone", user, ctxID)
		}
	}
	if !hasSession(t, s, "agent-kourier", "active") {
		t.Error("the active context's session is gone")
	}
	for id, caller := range map[string]context.Context{"busy-working": kourier, "other-done": other, "active-done": kourier} {
		if _, err := s.Get(caller, a2a.TaskID(id)); err != nil {
			t.Errorf("%s: %v, want kept", id, err)
		}
	}
	// The context's next message starts it afresh: a new session, its own activity.
	adkSession(t, s, "agent-kourier", "idle")
	mustCreate(t, s, task("idle-again", "idle", a2a.TaskStateCompleted))
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-90*time.Minute)); err != nil || n != 0 {
		t.Fatalf("a second DeleteIdleContexts = %d, %v; want 0", n, err)
	}
}

// A message to a context marks it active before its turn writes anything, so the idle sweep keeps the context the
// message is for; a context only the touch knows is swept once it is idle in turn.
func testTouchContext(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	mustCreate(t, s, task("old-done", "touched", a2a.TaskStateCompleted))
	adkSession(t, s, "agent-kourier", "touched")
	other := AsCaller("someone-else")
	if _, err := s.Create(other, task("other-done", "touched", a2a.TaskStateCompleted)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	if err := s.TouchContext(kourier, "touched"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchContext(kourier, "new"); err != nil {
		t.Fatal(err)
	}
	// Only the caller's context of that ID is touched: the other caller's is idle and goes.
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("DeleteIdleContexts = %d, %v; want the other caller's context alone", n, err)
	}
	if _, err := s.Get(kourier, "old-done"); err != nil {
		t.Errorf("the touched context's task: %v, want kept", err)
	}
	if !hasSession(t, s, "agent-kourier", "touched") {
		t.Error("the touched context's session is gone")
	}
	if _, err := s.Get(other, "other-done"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("the other caller's idle task: %v, want deleted", err)
	}
	clk.Advance(2 * time.Hour)
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-time.Hour)); err != nil || n != 2 {
		t.Fatalf("DeleteIdleContexts once both are idle = %d, %v; want 2", n, err)
	}
}

// ADK's sessions keep their events on the store's database.
func testADKSessions(t *testing.T, h Harness) {
	s, _ := open(t, h)
	adkSession(t, s, "agent-kourier", "c1")
	got, err := s.Sessions().Get(context.Background(), &session.GetRequest{AppName: store.AppName, UserID: "agent-kourier", SessionID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := got.Session.Events().Len(); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
}

func testReopen(t *testing.T, h Harness) {
	clk := NewClock()
	loc := h.New(t)
	s := h.Open(t, loc, clk.Now)
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateInputRequired))
	adkSession(t, s, "agent-kourier", "c1")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}

	again := h.Open(t, loc, clk.Now)
	if got, err := again.Get(kourier, "t1"); err != nil || got.Task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("Get after reopen: %+v, %v", got, err)
	}
	if got, err := again.MessageTask(kourier, "msg-t1"); err != nil || got == nil || got.ID != "t1" {
		t.Fatalf("MessageTask after reopen: %+v %v", got, err)
	}
	if !hasSession(t, again, "agent-kourier", "c1") {
		t.Fatal("the ADK session is gone after reopen")
	}
}
