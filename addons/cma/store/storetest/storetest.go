// Package storetest is the conformance suite that the adapter's SQLite and Postgres stores both run, so that the
// adapter can rely on the same behaviour from either.
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

	"github.com/agentkourier/agent-kourier/addons/cma/store"
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
		{"ListFilters", testListFilters},
		{"ListPages", testListPages},
		{"ListShapes", testListShapes},
		{"ListRefuses", testListRefuses},
		{"TasksInStates", testTasksInStates},
		{"DeleteSettledBefore", testDeleteSettledBefore},
		{"Messages", testMessages},
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

// asCaller is a context whose call runs as the authenticated user name.
func asCaller(name string) context.Context {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx
}

// task is a task with a history message, an artifact and metadata, so that a round trip covers each part of one.
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
	if _, err := s.Create(context.Background(), tk); err != nil {
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
	v, err := s.Create(asCaller("agent-kourier"), want)
	if err != nil || v != 1 {
		t.Fatalf("Create = %d, %v; want 1", v, err)
	}
	got, err := s.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Version != 1 || got.User != "agent-kourier" {
		t.Errorf("version %d user %q, want 1 agent-kourier", got.Version, got.User)
	}
	same(t, got.Task, want)

	// A call with no authenticated user (the check is off) records no owner, and every caller sees every task.
	mustCreate(t, s, task("t2", "c1", a2a.TaskStateSubmitted, nil))
	if got, err := s.Get(asCaller("someone-else"), "t2"); err != nil || got.User != "" {
		t.Fatalf("Get t2 = %+v, %v; want no owner", got, err)
	}
}

func testCreateTwice(t *testing.T, h Harness) {
	s, _ := open(t, h)
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateSubmitted, nil))
	if _, err := s.Create(context.Background(), task("t1", "c2", a2a.TaskStateWorking, nil)); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("second Create: %v, want ErrTaskAlreadyExists", err)
	}
	got, err := s.Get(context.Background(), "t1")
	if err != nil || got.Task.ContextID != "c1" {
		t.Fatalf("the second Create changed the task: %+v, %v", got, err)
	}
}

func testGetMissing(t *testing.T, h Harness) {
	s, _ := open(t, h)
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get: %v, want ErrTaskNotFound", err)
	}
}

func testUpdate(t *testing.T, h Harness) {
	s, _ := open(t, h)
	ctx := asCaller("agent-kourier")
	if _, err := s.Create(ctx, task("t1", "c1", a2a.TaskStateSubmitted, nil)); err != nil {
		t.Fatal(err)
	}
	working := task("t1", "c1", a2a.TaskStateWorking, nil)
	v, err := s.Update(context.Background(), &taskstore.UpdateRequest{Task: working, PrevVersion: 1})
	if err != nil || v != 2 {
		t.Fatalf("Update = %d, %v; want 2", v, err)
	}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: working, PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale Update: %v, want ErrConcurrentModification", err)
	}
	done := task("t1", "c1", a2a.TaskStateCompleted, nil)
	if v, err := s.Update(ctx, &taskstore.UpdateRequest{Task: done, PrevVersion: taskstore.TaskVersionMissing}); err != nil || v != 3 {
		t.Fatalf("unversioned Update = %d, %v; want 3", v, err)
	}
	got, err := s.Get(ctx, "t1")
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
		_, err := s.Update(context.Background(), &taskstore.UpdateRequest{Task: task("nope", "c1", a2a.TaskStateWorking, nil), PrevVersion: prev})
		if !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("Update with prev %d: %v, want ErrTaskNotFound", prev, err)
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
			resp, err := s.List(context.Background(), &req)
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
		resp, err := s.List(context.Background(), &a2a.ListTasksRequest{PageSize: 3, PageToken: token})
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
	// Newest first: the last created tasks come first.
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
	resp, err := s.List(context.Background(), &a2a.ListTasksRequest{})
	if err != nil || len(resp.Tasks) != 1 {
		t.Fatalf("List: %+v, %v", resp, err)
	}
	if got := resp.Tasks[0]; got.Artifacts != nil || len(got.History) != 4 {
		t.Errorf("default list: %d artifacts, %d history; want none and all 4", len(got.Artifacts), len(got.History))
	}
	two := 2
	resp, err = s.List(context.Background(), &a2a.ListTasksRequest{HistoryLength: &two, IncludeArtifacts: true})
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
	for name, req := range map[string]*a2a.ListTasksRequest{
		"page size 0 is the default, 101 is too many": {PageSize: 101},
		"negative page size":                          {PageSize: -1},
		"bad page token":                              {PageToken: "not-a-token"},
	} {
		if _, err := s.List(context.Background(), req); err == nil {
			t.Errorf("%s: List accepted it", name)
		}
	}
}

func testTasksInStates(t *testing.T, h Harness) {
	s, _ := open(t, h)
	for _, tk := range []*a2a.Task{
		task("sub", "c1", a2a.TaskStateSubmitted, nil),
		task("work", "c1", a2a.TaskStateWorking, nil),
		task("ask", "c2", a2a.TaskStateInputRequired, nil),
		task("done", "c2", a2a.TaskStateCompleted, nil),
	} {
		mustCreate(t, s, tk)
	}
	got, err := s.TasksInStates(context.Background(), a2a.TaskStateSubmitted, a2a.TaskStateWorking)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, st := range got {
		if st.Version != 1 {
			t.Errorf("%s version = %d, want 1", st.Task.ID, st.Version)
		}
		names = append(names, string(st.Task.ID))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"sub", "work"}) {
		t.Fatalf("tasks = %v, want sub and work", names)
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
		if err := s.RecordMessage(ctx, "msg-"+string(tk.ID), tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := clk.Now().Add(time.Nanosecond)
	clk.Advance(time.Hour)
	mustCreate(t, s, task("new-done", "c1", a2a.TaskStateCompleted, nil))
	if err := s.RecordMessage(ctx, "msg-new-done", "new-done"); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteSettledBefore(ctx, cutoff)
	if err != nil || n != 4 {
		t.Fatalf("DeleteSettledBefore = %d, %v; want 4", n, err)
	}
	for _, id := range []string{"old-done", "old-failed", "old-canceled", "old-rejected"} {
		if _, err := s.Get(ctx, a2a.TaskID(id)); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("%s: %v, want deleted", id, err)
		}
		if _, ok, err := s.MessageTask(ctx, "msg-"+id); err != nil || ok {
			t.Errorf("message of %s: ok %v, %v; want deleted with its task", id, ok, err)
		}
	}
	for _, id := range []string{"old-paused", "old-working", "new-done"} {
		if _, err := s.Get(ctx, a2a.TaskID(id)); err != nil {
			t.Errorf("%s: %v, want kept", id, err)
		}
		if _, ok, err := s.MessageTask(ctx, "msg-"+id); err != nil || !ok {
			t.Errorf("message of %s: ok %v, %v; want kept", id, ok, err)
		}
	}
}

func testMessages(t *testing.T, h Harness) {
	s, _ := open(t, h)
	ctx := context.Background()
	if _, ok, err := s.MessageTask(ctx, "m1"); err != nil || ok {
		t.Fatalf("MessageTask of an unknown message: ok %v, %v", ok, err)
	}
	if err := s.RecordMessage(ctx, "m1", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMessage(ctx, "m1", "t2"); err != nil {
		t.Fatalf("a second record: %v", err)
	}
	if id, ok, err := s.MessageTask(ctx, "m1"); err != nil || !ok || id != "t1" {
		t.Fatalf("MessageTask = %q, %v, %v; want the first task, t1", id, ok, err)
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
	got := sessions(t, s)
	if len(got) != 2 || got["c1"].SessionID != "sesn_3" || !got["c1"].LastActive.Equal(t0.Add(time.Hour)) || got["c2"].SessionID != "sesn_2" {
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
	for _, tk := range []*a2a.Task{task("c1-done", "c1", a2a.TaskStateCompleted, nil), task("c1-paused", "c1", a2a.TaskStateInputRequired, nil), task("c2-done", "c2", a2a.TaskStateCompleted, nil)} {
		mustCreate(t, s, tk)
		if err := s.RecordMessage(ctx, "msg-"+string(tk.ID), tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := clk.Now().Add(time.Nanosecond)
	clk.Advance(time.Minute)
	mustCreate(t, s, task("c1-after", "c1", a2a.TaskStateSubmitted, nil)) // a turn that started after the archive

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
	for id, kept := range map[string]bool{"c1-done": false, "c1-paused": false, "c1-after": true, "c2-done": true} {
		_, err := s.Get(ctx, a2a.TaskID(id))
		if kept != (err == nil) {
			t.Errorf("%s: %v, want kept %v", id, err, kept)
		}
	}
	if _, ok, _ := s.MessageTask(ctx, "msg-c1-done"); ok {
		t.Error("the message of a forgotten task is kept")
	}
	if _, ok, _ := s.MessageTask(ctx, "msg-c2-done"); !ok {
		t.Error("the message of another context's task is gone")
	}
}

func testReopen(t *testing.T, h Harness) {
	clk := NewClock()
	loc := h.New(t)
	s := h.Open(t, loc, clk.Now)
	ctx := context.Background()
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateInputRequired, nil))
	if err := s.RecordMessage(ctx, "m1", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSession(ctx, store.Session{ContextID: "c1", SessionID: "sesn_1", LastActive: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}

	again := h.Open(t, loc, clk.Now)
	if got, err := again.Get(ctx, "t1"); err != nil || got.Task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("Get after reopen: %+v, %v", got, err)
	}
	if id, ok, err := again.MessageTask(ctx, "m1"); err != nil || !ok || id != "t1" {
		t.Fatalf("MessageTask after reopen: %q %v %v", id, ok, err)
	}
	if got := sessions(t, again); got["c1"].SessionID != "sesn_1" || !got["c1"].LastActive.Equal(clk.Now()) {
		t.Fatalf("sessions after reopen: %+v", got)
	}
}
