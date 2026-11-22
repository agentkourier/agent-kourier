// Package storetest is the conformance suite that the agent's SQLite and Postgres stores both run, so that the agent
// can rely on the same behaviour from either.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
		{"CreateAndGet", testCreateAndGet},
		{"CreateTwice", testCreateTwice},
		{"GetMissing", testGetMissing},
		{"Update", testUpdate},
		{"UpdateMissing", testUpdateMissing},
		{"RefusesWithoutACaller", testRefusesWithoutACaller},
		{"ListFilters", testListFilters},
		{"ListPages", testListPages},
		{"ListShapes", testListShapes},
		{"ListRefuses", testListRefuses},
		{"Messages", testMessages},
		{"SettleInterrupted", testSettleInterrupted},
		{"DeleteSettledBefore", testDeleteSettledBefore},
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

// task is a task with a user message, an artifact and metadata, so that a round trip covers each part of one.
func task(id, contextID string, state a2a.TaskState, statusAt *time.Time) *a2a.Task {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello "+id), a2a.NewDataPart(map[string]any{"n": 1.0}))
	msg.ID, msg.TaskID, msg.ContextID = "msg-"+id, a2a.TaskID(id), contextID
	return &a2a.Task{
		ID: a2a.TaskID(id), ContextID: contextID,
		Status:    a2a.TaskStatus{State: state, Timestamp: statusAt},
		History:   []*a2a.Message{msg},
		Artifacts: []*a2a.Artifact{{ID: a2a.ArtifactID("art-" + id), Parts: a2a.ContentParts{a2a.NewTextPart("out " + id)}}},
		Metadata:  map[string]any{"k": "v\x00with a NUL"},
	}
}

func mustCreate(t *testing.T, s *store.Store, tk *a2a.Task) {
	t.Helper()
	if _, err := s.Create(kourier, tk); err != nil {
		t.Fatalf("Create %s: %v", tk.ID, err)
	}
}

func same(t *testing.T, got, want *a2a.Task) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(g) != string(w) {
		t.Fatalf("task =\n%s\nwant\n%s", g, w)
	}
}

func testCreateAndGet(t *testing.T, h Harness) {
	s, _ := open(t, h)
	at := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	want := task("t1", "c1", a2a.TaskStateSubmitted, &at)
	v, err := s.Create(kourier, want)
	if err != nil || v != 1 {
		t.Fatalf("Create = %d, %v; want 1", v, err)
	}
	got, err := s.Get(kourier, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Version != 1 || got.User != "agent-kourier" {
		t.Errorf("version %d user %q, want 1 agent-kourier", got.Version, got.User)
	}
	same(t, got.Task, want)
	// Another caller does not see it.
	if _, err := s.Get(AsCaller("someone-else"), "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get as another caller: %v, want ErrTaskNotFound", err)
	}
}

func testCreateTwice(t *testing.T, h Harness) {
	s, _ := open(t, h)
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateSubmitted, nil))
	for name, ctx := range map[string]context.Context{"same caller": kourier, "another caller": AsCaller("someone-else")} {
		tk := task("t1", "c2", a2a.TaskStateWorking, nil)
		tk.History[0].ID = "msg-other"
		if _, err := s.Create(ctx, tk); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
			t.Fatalf("%s: second Create: %v, want ErrTaskAlreadyExists", name, err)
		}
	}
	got, err := s.Get(kourier, "t1")
	if err != nil || got.Task.ContextID != "c1" {
		t.Fatalf("the second Create changed the task: %+v, %v", got, err)
	}
}

func testGetMissing(t *testing.T, h Harness) {
	s, _ := open(t, h)
	if _, err := s.Get(kourier, "nope"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get: %v, want ErrTaskNotFound", err)
	}
}

func testUpdate(t *testing.T, h Harness) {
	s, _ := open(t, h)
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateSubmitted, nil))
	working := task("t1", "c1", a2a.TaskStateWorking, nil)
	v, err := s.Update(kourier, &taskstore.UpdateRequest{Task: working, PrevVersion: 1})
	if err != nil || v != 2 {
		t.Fatalf("Update = %d, %v; want 2", v, err)
	}
	if _, err := s.Update(kourier, &taskstore.UpdateRequest{Task: working, PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale Update: %v, want ErrConcurrentModification", err)
	}
	if _, err := s.Update(AsCaller("someone-else"), &taskstore.UpdateRequest{Task: working, PrevVersion: 2}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Update as another caller: %v, want ErrTaskNotFound", err)
	}
	done := task("t1", "c1", a2a.TaskStateCompleted, nil)
	if v, err := s.Update(kourier, &taskstore.UpdateRequest{Task: done, PrevVersion: taskstore.TaskVersionMissing}); err != nil || v != 3 {
		t.Fatalf("unversioned Update = %d, %v; want 3", v, err)
	}
	got, err := s.Get(kourier, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 || got.User != "agent-kourier" {
		t.Errorf("version %d user %q, want 3 and the creator kept", got.Version, got.User)
	}
	same(t, got.Task, done)
}

func testUpdateMissing(t *testing.T, h Harness) {
	s, _ := open(t, h)
	for _, prev := range []taskstore.TaskVersion{taskstore.TaskVersionMissing, 1} {
		_, err := s.Update(kourier, &taskstore.UpdateRequest{Task: task("nope", "c1", a2a.TaskStateWorking, nil), PrevVersion: prev})
		if !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("Update with prev %d: %v, want ErrTaskNotFound", prev, err)
		}
	}
}

// A call with no authenticated caller reads and writes nothing.
func testRefusesWithoutACaller(t *testing.T, h Harness) {
	s, _ := open(t, h)
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateSubmitted, nil))
	ctx := context.Background()
	for name, err := range map[string]error{
		"Create": func() error { _, err := s.Create(ctx, task("t2", "c1", a2a.TaskStateSubmitted, nil)); return err }(),
		"Update": func() error {
			_, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task("t1", "c1", a2a.TaskStateWorking, nil), PrevVersion: 1})
			return err
		}(),
		"Get":          func() error { _, err := s.Get(ctx, "t1"); return err }(),
		"List":         func() error { _, err := s.List(ctx, &a2a.ListTasksRequest{}); return err }(),
		"MessageTask":  func() error { _, err := s.MessageTask(ctx, "msg-t1"); return err }(),
		"TouchContext": s.TouchContext(ctx, "c1"),
	} {
		if !errors.Is(err, a2a.ErrUnauthenticated) {
			t.Errorf("%s: %v, want ErrUnauthenticated", name, err)
		}
	}
}

func ids(tasks []*a2a.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, tk := range tasks {
		out = append(out, string(tk.ID))
	}
	return out
}

func testListFilters(t *testing.T, h Harness) {
	s, clk := open(t, h)
	early := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for _, tk := range []*a2a.Task{
		task("a", "c1", a2a.TaskStateCompleted, &early),
		task("b", "c1", a2a.TaskStateWorking, &late),
		task("c", "c2", a2a.TaskStateCompleted, &late),
		task("d", "c2", a2a.TaskStateInputRequired, nil),
	} {
		mustCreate(t, s, tk)
		clk.Advance(time.Second)
	}
	if _, err := s.Create(AsCaller("someone-else"), task("e", "c1", a2a.TaskStateCompleted, &late)); err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		req  a2a.ListTasksRequest
		want []string
	}{
		{"all, newest first", a2a.ListTasksRequest{}, []string{"d", "c", "b", "a"}},
		{"context", a2a.ListTasksRequest{ContextID: "c1"}, []string{"b", "a"}},
		{"status", a2a.ListTasksRequest{Status: a2a.TaskStateCompleted}, []string{"c", "a"}},
		{"context and status", a2a.ListTasksRequest{ContextID: "c2", Status: a2a.TaskStateCompleted}, []string{"c"}},
		{"status after, untimed kept", a2a.ListTasksRequest{StatusTimestampAfter: &after}, []string{"d", "c", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			resp, err := s.List(kourier, &req)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := ids(resp.Tasks); !slices.Equal(got, tc.want) {
				t.Errorf("tasks = %v, want %v", got, tc.want)
			}
			if resp.TotalSize != len(tc.want) || resp.NextPageToken != "" {
				t.Errorf("total %d next %q, want %d and no next page", resp.TotalSize, resp.NextPageToken, len(tc.want))
			}
		})
	}
}

func testListPages(t *testing.T, h Harness) {
	s, clk := open(t, h)
	for i := range 7 {
		id := fmt.Sprintf("t%d", i)
		mustCreate(t, s, task(id, "c1", a2a.TaskStateCompleted, nil))
		if i%2 == 1 {
			clk.Advance(time.Second) // some tasks share an update time: the ID breaks the tie
		}
	}
	var got []string
	token := ""
	for page := 0; ; page++ {
		resp, err := s.List(kourier, &a2a.ListTasksRequest{PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		if resp.TotalSize != 7 || resp.PageSize != 3 {
			t.Errorf("page %d: total %d size %d, want 7 and 3", page, resp.TotalSize, resp.PageSize)
		}
		got = append(got, ids(resp.Tasks)...)
		if token = resp.NextPageToken; token == "" {
			break
		}
		if page > 5 {
			t.Fatal("paging does not end")
		}
	}
	if len(got) != 7 {
		t.Fatalf("pages held %v, want 7 tasks", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("pages repeat %s: %v", id, got)
		}
		seen[id] = true
	}
	if got[0] != "t6" || got[6] != "t0" {
		t.Errorf("order = %v, want newest first", got)
	}
}

func testListShapes(t *testing.T, h Harness) {
	s, _ := open(t, h)
	tk := task("t1", "c1", a2a.TaskStateCompleted, nil)
	for i := range 3 {
		m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("reply %d", i)))
		tk.History = append(tk.History, m)
	}
	mustCreate(t, s, tk)
	resp, err := s.List(kourier, &a2a.ListTasksRequest{})
	if err != nil || len(resp.Tasks) != 1 {
		t.Fatalf("List: %+v, %v", resp, err)
	}
	if got := resp.Tasks[0]; got.Artifacts != nil || len(got.History) != 4 {
		t.Errorf("default list: %d artifacts, %d history; want none and all 4", len(got.Artifacts), len(got.History))
	}
	two := 2
	resp, err = s.List(kourier, &a2a.ListTasksRequest{HistoryLength: &two, IncludeArtifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Tasks[0]
	if len(got.Artifacts) != 1 || len(got.History) != 2 || got.History[1].Parts[0].Text() != "reply 2" {
		t.Errorf("artifacts %d history %d, want 1 artifact and the last 2 messages", len(got.Artifacts), len(got.History))
	}
}

func testListRefuses(t *testing.T, h Harness) {
	s, _ := open(t, h)
	negative := -1
	for name, req := range map[string]*a2a.ListTasksRequest{
		"page size 0 is the default, 101 is too many": {PageSize: 101},
		"negative page size":                          {PageSize: -1},
		"bad page token":                              {PageToken: "not-a-token"},
		"negative history":                            {HistoryLength: &negative},
	} {
		if _, err := s.List(kourier, req); err == nil {
			t.Errorf("%s: List accepted it", name)
		}
	}
}

// The IDs of a task's user messages are recorded with it, by caller, and one cannot move to another task.
func testMessages(t *testing.T, h Harness) {
	s, _ := open(t, h)
	if got, err := s.MessageTask(kourier, "msg-t1"); err != nil || got != nil {
		t.Fatalf("MessageTask of an unknown message: %+v, %v", got, err)
	}
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateInputRequired, nil))
	if got, err := s.MessageTask(kourier, "msg-t1"); err != nil || got == nil || got.ID != "t1" {
		t.Fatalf("MessageTask = %+v, %v; want t1", got, err)
	}
	if got, err := s.MessageTask(AsCaller("someone-else"), "msg-t1"); err != nil || got != nil {
		t.Fatalf("MessageTask as another caller = %+v, %v; want none", got, err)
	}
	// An answer joins the task: its ID is recorded by the update.
	answered := task("t1", "c1", a2a.TaskStateWorking, nil)
	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("checkout"))
	answer.ID = "answer-1"
	agent := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("noted"))
	agent.ID = "agent-1"
	answered.History = append(answered.History, answer, agent)
	if _, err := s.Update(kourier, &taskstore.UpdateRequest{Task: answered, PrevVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MessageTask(kourier, "answer-1"); err != nil || got == nil || got.ID != "t1" {
		t.Fatalf("MessageTask of the answer = %+v, %v; want t1", got, err)
	}
	if got, err := s.MessageTask(kourier, "agent-1"); err != nil || got != nil {
		t.Fatalf("an agent message's ID was recorded: %+v, %v", got, err)
	}
	// A task that carries a message ID another task holds is refused, and the ID stays where it was.
	thief := task("t2", "c1", a2a.TaskStateSubmitted, nil)
	thief.History[0].ID = "answer-1"
	if _, err := s.Create(kourier, thief); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("Create with a held message ID: %v, want ErrTaskAlreadyExists", err)
	}
	if _, err := s.Get(kourier, "t2"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("the refused task was kept: %v", err)
	}
	// Another caller's message IDs are its own.
	if _, err := s.Create(AsCaller("someone-else"), thief); err != nil {
		t.Fatalf("another caller's message ID: %v", err)
	}
}

func testSettleInterrupted(t *testing.T, h Harness) {
	s, clk := open(t, h)
	for _, tk := range []*a2a.Task{
		task("sub", "c1", a2a.TaskStateSubmitted, nil),
		task("work", "c1", a2a.TaskStateWorking, nil),
		task("ask", "c2", a2a.TaskStateInputRequired, nil),
		task("done", "c2", a2a.TaskStateCompleted, nil),
	} {
		mustCreate(t, s, tk)
	}
	clk.Advance(time.Minute)
	n, err := s.SettleInterrupted(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("SettleInterrupted = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]a2a.TaskState{
		"sub": a2a.TaskStateFailed, "work": a2a.TaskStateFailed, "ask": a2a.TaskStateInputRequired, "done": a2a.TaskStateCompleted,
	} {
		got, err := s.Get(kourier, a2a.TaskID(id))
		if err != nil {
			t.Fatal(err)
		}
		if got.Task.Status.State != want {
			t.Errorf("%s = %s, want %s", id, got.Task.Status.State, want)
		}
		if want != a2a.TaskStateFailed {
			continue
		}
		if got.Version != 2 || got.Task.Status.Message == nil || got.Task.Status.Message.Parts[0].Text() != store.InterruptedMessage ||
			got.Task.Status.Timestamp == nil || !got.Task.Status.Timestamp.Equal(clk.Now()) {
			t.Errorf("%s: version %d status %+v, want version 2 and the interrupted message now", id, got.Version, got.Task.Status)
		}
	}
	resp, err := s.List(kourier, &a2a.ListTasksRequest{Status: a2a.TaskStateFailed})
	if err != nil || len(resp.Tasks) != 2 {
		t.Fatalf("failed tasks by state: %+v, %v", resp, err)
	}
	if n, err := s.SettleInterrupted(context.Background()); err != nil || n != 0 {
		t.Fatalf("a second SettleInterrupted = %d, %v; want 0", n, err)
	}
}

func testDeleteSettledBefore(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	for _, tk := range []*a2a.Task{
		task("old-done", "c1", a2a.TaskStateCompleted, nil),
		task("old-failed", "c1", a2a.TaskStateFailed, nil),
		task("old-canceled", "c1", a2a.TaskStateCanceled, nil),
		task("old-rejected", "c1", a2a.TaskStateRejected, nil),
		task("old-paused", "c1", a2a.TaskStateInputRequired, nil),
		task("old-working", "c1", a2a.TaskStateWorking, nil),
	} {
		mustCreate(t, s, tk)
	}
	cutoff := clk.Now().Add(time.Nanosecond)
	clk.Advance(time.Hour)
	mustCreate(t, s, task("new-done", "c1", a2a.TaskStateCompleted, nil))

	n, err := s.DeleteSettledBefore(ctx, cutoff)
	if err != nil || n != 4 {
		t.Fatalf("DeleteSettledBefore = %d, %v; want 4", n, err)
	}
	for _, id := range []string{"old-done", "old-failed", "old-canceled", "old-rejected"} {
		if _, err := s.Get(kourier, a2a.TaskID(id)); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("%s: %v, want deleted", id, err)
		}
		if got, err := s.MessageTask(kourier, "msg-"+id); err != nil || got != nil {
			t.Errorf("message of %s: %+v, %v; want deleted with its task", id, got, err)
		}
	}
	for _, id := range []string{"old-paused", "old-working", "new-done"} {
		if _, err := s.Get(kourier, a2a.TaskID(id)); err != nil {
			t.Errorf("%s: %v, want kept", id, err)
		}
		if got, err := s.MessageTask(kourier, "msg-"+id); err != nil || got == nil {
			t.Errorf("message of %s: %+v, %v; want kept", id, got, err)
		}
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
	mustCreate(t, s, task("idle-done", "idle", a2a.TaskStateCompleted, nil))
	mustCreate(t, s, task("idle-paused", "idle", a2a.TaskStateInputRequired, nil))
	adkSession(t, s, "agent-kourier", "idle")
	// Idle by its last update, but a turn is running in it.
	mustCreate(t, s, task("busy-working", "busy", a2a.TaskStateWorking, nil))
	adkSession(t, s, "agent-kourier", "busy")
	// Another caller's context of the same ID, active later.
	adkSession(t, s, "someone-else", "idle")
	clk.Advance(time.Hour)
	if _, err := s.Create(other, task("other-done", "idle", a2a.TaskStateCompleted, nil)); err != nil {
		t.Fatal(err)
	}
	// Active: its first task is old, its last update is not.
	mustCreate(t, s, task("active-done", "active", a2a.TaskStateCompleted, nil))
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
	mustCreate(t, s, task("idle-again", "idle", a2a.TaskStateCompleted, nil))
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-90*time.Minute)); err != nil || n != 0 {
		t.Fatalf("a second DeleteIdleContexts = %d, %v; want 0", n, err)
	}
}

// A message to a context marks it active before its turn writes anything, so the idle sweep keeps the context the
// message is for; a context only the touch knows is swept once it is idle in turn.
func testTouchContext(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	mustCreate(t, s, task("old-done", "touched", a2a.TaskStateCompleted, nil))
	adkSession(t, s, "agent-kourier", "touched")
	other := AsCaller("someone-else")
	if _, err := s.Create(other, task("other-done", "touched", a2a.TaskStateCompleted, nil)); err != nil {
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
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateInputRequired, nil))
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
