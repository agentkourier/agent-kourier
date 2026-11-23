// Package storetest is the conformance suite of what the adapter's store adds to the kit's task state, which the kit's
// own suite (addons/kit/store/storetest) covers: each context's CMA session, kept as the context's ref, and the tasks
// of an archived session forgotten. The adapter's SQLite and Postgres stores both run it, so that the adapter can rely
// on the same behaviour from either.
package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
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
		{"Sessions", testSessions},
		{"ForgetContext", testForgetContext},
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

// AsKourier is a context whose call runs as the adapter's one caller, as the caller check runs every call.
func AsKourier() context.Context {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(auth.Caller, nil)
	return ctx
}

// task is a task with a user message, as the adapter's server creates one.
func task(id, contextID string, state a2a.TaskState) *a2a.Task {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello "+id))
	msg.ID, msg.TaskID, msg.ContextID = "msg-"+id, a2a.TaskID(id), contextID
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: contextID, Status: a2a.TaskStatus{State: state}, History: []*a2a.Message{msg}}
}

func mustCreate(t *testing.T, s *store.Store, tk *a2a.Task) {
	t.Helper()
	if _, err := s.Create(AsKourier(), tk); err != nil {
		t.Fatalf("Create %s: %v", tk.ID, err)
	}
}

func sessions(t *testing.T, s *store.Store) map[string]store.Session {
	t.Helper()
	all, err := s.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Session{}
	for _, ss := range all {
		out[ss.ContextID] = ss
	}
	return out
}

func testSessions(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	t0 := clk.Now()
	for _, ss := range []store.Session{
		{ContextID: "c1", SessionID: "sesn_1", LastActive: t0},
		{ContextID: "c2", SessionID: "sesn_2", LastActive: t0.Add(time.Minute)},
	} {
		if err := s.PutSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutSession(ctx, store.Session{ContextID: "c1", SessionID: "sesn_3", LastActive: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// A task's create leaves its context's activity alone, and a context with no session is none of Sessions'.
	clk.Advance(5 * time.Hour)
	mustCreate(t, s, task("t1", "c2", a2a.TaskStateWorking))
	mustCreate(t, s, task("t2", "c3", a2a.TaskStateWorking))
	got := sessions(t, s)
	if len(got) != 2 || got["c1"].SessionID != "sesn_3" || !got["c1"].LastActive.Equal(t0.Add(time.Hour)) ||
		got["c2"].SessionID != "sesn_2" || !got["c2"].LastActive.Equal(t0.Add(time.Minute)) {
		t.Fatalf("sessions = %+v", got)
	}
	// A touch for a session the context no longer has is dropped.
	if err := s.TouchSession(ctx, "c1", "sesn_1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, "c2", "sesn_2", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got = sessions(t, s)
	if !got["c1"].LastActive.Equal(t0.Add(time.Hour)) || !got["c2"].LastActive.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("after touches: %+v", got)
	}
}

func testForgetContext(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	start := clk.Now()
	clk.Advance(time.Second)
	for _, ss := range []store.Session{{ContextID: "c1", SessionID: "sesn_1"}, {ContextID: "c2", SessionID: "sesn_2"}} {
		ss.LastActive = clk.Now()
		if err := s.PutSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
	}
	for _, tk := range []*a2a.Task{task("c1-done", "c1", a2a.TaskStateCompleted), task("c1-paused", "c1", a2a.TaskStateInputRequired), task("c2-done", "c2", a2a.TaskStateCompleted)} {
		mustCreate(t, s, tk)
	}
	cutoff := clk.Now().Add(time.Nanosecond)
	clk.Advance(time.Minute)
	mustCreate(t, s, task("c1-after", "c1", a2a.TaskStateSubmitted)) // a turn that started after the archive

	// A context whose session was replaced keeps its new session, and the tasks since.
	if err := s.ForgetContext(ctx, "c2", "sesn_old", start); err != nil {
		t.Fatal(err)
	}
	if got := sessions(t, s); got["c2"].SessionID != "sesn_2" {
		t.Fatalf("forgetting a replaced session removed the context's current one: %+v", got)
	}

	if err := s.ForgetContext(ctx, "c1", "sesn_1", cutoff); err != nil {
		t.Fatal(err)
	}
	if got := sessions(t, s); len(got) != 1 || got["c2"].SessionID != "sesn_2" {
		t.Fatalf("sessions = %+v, want c2's alone", got)
	}
	kourier := AsKourier()
	for id, kept := range map[string]bool{"c1-done": false, "c1-paused": false, "c1-after": true, "c2-done": true} {
		_, err := s.Get(kourier, a2a.TaskID(id))
		if kept != (err == nil) {
			t.Errorf("%s: %v, want kept %v", id, err, kept)
		}
	}
	if got, _ := s.MessageTask(kourier, "msg-c1-done"); got != nil {
		t.Error("the message of a forgotten task is kept")
	}
	if got, _ := s.MessageTask(kourier, "msg-c2-done"); got == nil {
		t.Error("the message of another context's task is gone")
	}
}

func testReopen(t *testing.T, h Harness) {
	clk := NewClock()
	loc := h.New(t)
	s := h.Open(t, loc, clk.Now)
	kourier := AsKourier()
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateInputRequired))
	if err := s.PutSession(context.Background(), store.Session{ContextID: "c1", SessionID: "sesn_1", LastActive: clk.Now()}); err != nil {
		t.Fatal(err)
	}
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
	if got := sessions(t, again); got["c1"].SessionID != "sesn_1" || !got["c1"].LastActive.Equal(clk.Now()) {
		t.Fatalf("sessions after reopen: %+v", got)
	}
}
