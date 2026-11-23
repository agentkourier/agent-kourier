// Package storetest is the conformance suite of the kit's task state (store.Store): each add-on runs Run from a test of
// its store package, on SQLite under the driver it links and on Postgres, so that every add-on, and the kit, can rely on
// the same behaviour from either. Only tests import it.
package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/store"
)

// Harness opens an add-on's databases on one backend.
type Harness struct {
	// New returns a fresh, empty location (a file path, a schema), removed when the test ends.
	New func(t *testing.T) string
	// Open opens the add-on's database at location as the add-on does (its driver, its Prepare and its migrations),
	// logging to log, and on Postgres takes the lease, so that it writes. Opening a location again, after Close, finds
	// what the last database wrote there. The harness closes the database when the test ends; a test may close it
	// first.
	Open func(t *testing.T, location string, log *slog.Logger) *store.DB
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

// AsCaller is a context whose A2A call runs as the authenticated caller name.
func AsCaller(name string) context.Context {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx
}

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
		{"SettleInterruptedSkipsATask", testSettleInterruptedSkipsATask},
		{"SettleInterruptedStopsOnAStoreError", testSettleInterruptedStopsOnAStoreError},
		{"DeleteSettledBefore", testDeleteSettledBefore},
		{"ContextActivity", testContextActivity},
		{"ContextRefs", testContextRefs},
		{"ForgetContext", testForgetContext},
		{"DeleteIdleContexts", testDeleteIdleContexts},
		{"ReleaseContext", testReleaseContext},
		{"TouchContext", testTouchContext},
		{"Reopen", testReopen},
		{"EveryWriteIsFenced", testEveryWriteIsFenced},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

// kourier is the caller most tests run as; other, another caller.
var (
	kourier = AsCaller("agent-kourier")
	other   = AsCaller("someone-else")
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// open opens a fresh database and the task state on it, with ContextActivity on.
func open(t *testing.T, h Harness) (*store.Store, *Clock) {
	t.Helper()
	return openWith(t, h, store.TaskOptions{ContextActivity: true}, discard())
}

func openWith(t *testing.T, h Harness, opts store.TaskOptions, log *slog.Logger) (*store.Store, *Clock) {
	t.Helper()
	clk := NewClock()
	opts.Now = clk.Now
	return store.NewStore(h.Open(t, h.New(t), log), opts), clk
}

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
	if _, err := s.Get(other, "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get as another caller: %v, want ErrTaskNotFound", err)
	}
}

func testCreateTwice(t *testing.T, h Harness) {
	s, _ := open(t, h)
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateSubmitted, nil))
	for name, ctx := range map[string]context.Context{"same caller": kourier, "another caller": other} {
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
	if _, err := s.Update(other, &taskstore.UpdateRequest{Task: working, PrevVersion: 2}); !errors.Is(err, a2a.ErrTaskNotFound) {
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
	if got, err := s.Get(kourier, "t1"); err != nil || got.Version != 1 {
		t.Fatalf("a refused call changed the task: %+v, %v", got, err)
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
	if _, err := s.Create(other, task("e", "c1", a2a.TaskStateCompleted, &late)); err != nil {
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
	resp, err := s.List(kourier, &a2a.ListTasksRequest{})
	if err != nil || len(resp.Tasks) != 1 {
		t.Fatalf("List: %+v, %v", resp, err)
	}
	if got := resp.Tasks[0]; got.Artifacts != nil || len(got.History) != 4 {
		t.Errorf("default list: %d artifacts, %d history; want none and all 4", len(got.Artifacts), len(got.History))
	}
	two, zero := 2, 0
	resp, err = s.List(kourier, &a2a.ListTasksRequest{HistoryLength: &two, IncludeArtifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Tasks[0]
	if len(got.Artifacts) != 1 || len(got.History) != 2 || got.History[1].Parts[0].Text() != "reply 2" {
		t.Errorf("artifacts %d history %d, want 1 artifact and the last 2 messages", len(got.Artifacts), len(got.History))
	}
	if resp, err = s.List(kourier, &a2a.ListTasksRequest{HistoryLength: &zero}); err != nil || len(resp.Tasks[0].History) != 0 {
		t.Errorf("history length 0: %+v, %v; want no messages", resp, err)
	}
}

func testListRefuses(t *testing.T, h Harness) {
	s, _ := open(t, h)
	negative := -1
	for name, req := range map[string]*a2a.ListTasksRequest{
		"page size 0 is the default, 101 is too many": {PageSize: store.MaxPageSize + 1},
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
	if got, err := s.MessageTask(other, "msg-t1"); err != nil || got != nil {
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
	if got, err := s.MessageTask(kourier, "answer-1"); err != nil || got == nil || got.ID != "t1" {
		t.Fatalf("the held ID after the refusal = %+v, %v; want t1", got, err)
	}
	// Another caller's message IDs are its own.
	if _, err := s.Create(other, thief); err != nil {
		t.Fatalf("another caller's message ID: %v", err)
	}
}

// interruptedText is the agent message the suite fails interrupted tasks with.
const interruptedText = "Interrupted by a restart of the conformance suite."

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
	if _, err := s.Create(other, task("other-work", "c3", a2a.TaskStateWorking, nil)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	done, err := s.SettleInterrupted(context.Background(), interruptedText)
	if err != nil {
		t.Fatalf("SettleInterrupted: %v", err)
	}
	if got := ids(done.Failed); !slices.Equal(got, []string{"other-work", "sub", "work"}) || len(done.Unreadable) != 0 || done.Skipped != 0 ||
		done.Interrupted() != 3 {
		t.Fatalf("SettleInterrupted failed %v (%+v), want every caller's running tasks", got, done)
	}
	for _, tk := range done.Failed {
		if tk.Status.State != a2a.TaskStateFailed || tk.Status.Message == nil || tk.Status.Message.Parts[0].Text() != interruptedText {
			t.Errorf("%s as returned: %+v, want failed with the message", tk.ID, tk.Status)
		}
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
			if got.Version != 1 {
				t.Errorf("%s: version %d, want it untouched", id, got.Version)
			}
			continue
		}
		msg := got.Task.Status.Message
		if got.Version != 2 || msg == nil || msg.Role != a2a.MessageRoleAgent || msg.Parts[0].Text() != interruptedText ||
			got.Task.Status.Timestamp == nil || !got.Task.Status.Timestamp.Equal(clk.Now()) {
			t.Errorf("%s: version %d status %+v, want version 2 and the interrupted message now", id, got.Version, got.Task.Status)
		}
	}
	resp, err := s.List(kourier, &a2a.ListTasksRequest{Status: a2a.TaskStateFailed})
	if err != nil || len(resp.Tasks) != 2 {
		t.Fatalf("failed tasks by state: %+v, %v", resp, err)
	}
	if again, err := s.SettleInterrupted(context.Background(), interruptedText); err != nil || again.Interrupted() != 0 || again.Skipped != 0 {
		t.Fatalf("a second SettleInterrupted = %+v, %v; want none", again, err)
	}
}

// warnings is a log that keeps the messages of its WARN records.
type warnings struct {
	mu   sync.Mutex
	msgs []string
	keys []string
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnings) WithGroup(string) slog.Handler            { return w }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level != slog.LevelWarn {
		return nil
	}
	var b bytes.Buffer
	r.Attrs(func(a slog.Attr) bool { b.WriteString(a.Key + "=" + a.Value.String() + " "); return true })
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, r.Message)
	w.keys = append(w.keys, b.String())
	return nil
}

// A task whose stored record does not decode is failed by its columns alone, so that the retention deletes it, and
// logged; a task that changed since it was read is logged and left; the others are failed.
func testSettleInterruptedSkipsATask(t *testing.T, h Harness) {
	log := &warnings{}
	s, clk := openWith(t, h, store.TaskOptions{}, slog.New(log))
	ctx := context.Background()
	for _, id := range []string{"a-broken", "b-raced", "c-fine"} {
		mustCreate(t, s, task(id, "c1", a2a.TaskStateWorking, nil))
	}
	// A record that is JSON, which Postgres keeps, but no task.
	if err := s.Write(ctx, func(q store.Querier) error {
		_, err := q.Exec(ctx, `UPDATE a2a_tasks SET payload = ? WHERE id = ?`, []byte(`[1]`), "a-broken")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A task that changes between the read and its write: the fence of its write, the third fenced step after the
	// search's and a-broken's, moves it on first.
	fences := 0
	store.SetFenceForTest(s.DB, func(ctx context.Context, q store.Querier) error {
		if fences++; fences != 3 {
			return nil
		}
		_, err := q.Exec(ctx, `UPDATE a2a_tasks SET version = version + 1 WHERE id = ?`, "b-raced")
		return err
	})
	defer store.SetFenceForTest(s.DB, nil)

	done, err := s.SettleInterrupted(ctx, interruptedText)
	if err != nil {
		t.Fatalf("SettleInterrupted: %v, want the store's failures alone to stop it", err)
	}
	if got := ids(done.Failed); !slices.Equal(got, []string{"c-fine"}) || done.Skipped != 1 ||
		!slices.Equal(done.Unreadable, []a2a.TaskInfo{{TaskID: "a-broken", ContextID: "c1"}}) || done.Interrupted() != 2 {
		t.Fatalf("SettleInterrupted = %+v, want c-fine failed, a-broken failed unread and b-raced skipped", done)
	}
	store.SetFenceForTest(s.DB, nil)
	if got, err := s.Get(kourier, "b-raced"); err != nil || got.Task.Status.State != a2a.TaskStateWorking || got.Version != 2 {
		t.Fatalf("the task that changed: %+v, %v; want it as the change left it", got, err)
	}
	var state string
	var version, statusAt, updatedAt int64
	if err := s.Reader().QueryRow(ctx, `SELECT state, version, status_at, updated_at FROM a2a_tasks WHERE id = ?`, "a-broken").
		Scan(&state, &version, &statusAt, &updatedAt); err != nil || state != string(a2a.TaskStateFailed) || version != 2 ||
		statusAt != clk.Now().UnixNano() || updatedAt != clk.Now().UnixNano() {
		t.Fatalf("the unreadable task: %s version %d status_at %d updated_at %d, %v; want failed now, version 2", state, version, statusAt, updatedAt, err)
	}
	func() {
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.msgs) != 2 {
			t.Fatalf("WARN records %v, want one for each task not failed as read", log.msgs)
		}
		for i, want := range []struct{ id, msg string }{
			{"a-broken", "fail an unreadable interrupted task by its state alone"}, {"b-raced", "fail an interrupted task"},
		} {
			if log.msgs[i] != want.msg || !strings.Contains(log.keys[i], "task="+want.id+" ") || !strings.Contains(log.keys[i], "error=") {
				t.Errorf("WARN %d: %q %q, want %q with the task %s and its error", i, log.msgs[i], log.keys[i], want.msg, want.id)
			}
		}
	}()
	// The retention deletes the unreadable task as a settled one; a running task is no settled one.
	clk.Advance(time.Minute)
	if n, err := s.DeleteSettledBefore(ctx, clk.Now()); err != nil || n != 2 {
		t.Fatalf("DeleteSettledBefore = %d, %v; want a-broken and c-fine", n, err)
	}
	var left []string
	if err := s.Reader().Query(ctx, `SELECT id FROM a2a_tasks ORDER BY id`, nil, func(scan func(...any) error) error {
		var id string
		if err := scan(&id); err != nil {
			return err
		}
		left = append(left, id)
		return nil
	}); err != nil || !slices.Equal(left, []string{"b-raced"}) {
		t.Fatalf("tasks left: %v, %v; want b-raced alone", left, err)
	}
}

// errFenced stands for a deposed leader's refusal.
var errFenced = errors.New("fenced")

// A write that fails stops SettleInterrupted with an error: the store is at fault, not one task. Its search runs the
// fence too, so that a store that has lost the lease finds that out with nothing to settle.
func testSettleInterruptedStopsOnAStoreError(t *testing.T, h Harness) {
	for _, tc := range []struct {
		name  string
		tasks []*a2a.Task
	}{
		{"a task to settle", []*a2a.Task{task("t1", "c1", a2a.TaskStateWorking, nil)}},
		{"nothing to settle", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t, h)
			for _, tk := range tc.tasks {
				mustCreate(t, s, tk)
			}
			store.SetFenceForTest(s.DB, func(context.Context, store.Querier) error { return errFenced })
			defer store.SetFenceForTest(s.DB, nil)
			if _, err := s.SettleInterrupted(context.Background(), interruptedText); !errors.Is(err, errFenced) {
				t.Fatalf("SettleInterrupted: %v, want the fence's error", err)
			}
		})
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

func contexts(t *testing.T, s *store.Store) map[string]store.Context {
	t.Helper()
	all, err := s.Contexts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Context{}
	for _, c := range all {
		out[c.Owner+"/"+c.ContextID] = c
	}
	return out
}

// With ContextActivity, a task's Create and Update record its context as active, keeping the context's ref; without,
// they leave the contexts alone.
func testContextActivity(t *testing.T, h Harness) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("ContextActivity=%v", on), func(t *testing.T) {
			s, clk := openWith(t, h, store.TaskOptions{ContextActivity: on}, discard())
			ctx := context.Background()
			if err := s.PutContext(ctx, store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: clk.Now()}); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Minute)
			mustCreate(t, s, task("t1", "c1", a2a.TaskStateSubmitted, nil))
			mustCreate(t, s, task("t2", "c2", a2a.TaskStateSubmitted, nil))
			clk.Advance(time.Minute)
			if _, err := s.Update(kourier, &taskstore.UpdateRequest{Task: task("t1", "c1", a2a.TaskStateWorking, nil), PrevVersion: 1}); err != nil {
				t.Fatal(err)
			}
			got := contexts(t, s)
			start := NewClock().Now()
			if !on {
				if len(got) != 1 || got["agent-kourier/c1"] != (store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: start}) {
					t.Fatalf("contexts = %+v, want the put one alone, unchanged", got)
				}
				return
			}
			if len(got) != 2 || got["agent-kourier/c1"] != (store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: clk.Now()}) ||
				got["agent-kourier/c2"] != (store.Context{Owner: "agent-kourier", ContextID: "c2", LastActive: start.Add(time.Minute)}) {
				t.Fatalf("contexts = %+v, want c1 active at the update with its ref kept, and c2 at its create", got)
			}
		})
	}
}

// PutContext records a context's ref and activity, and TouchContextRef moves the activity while the ref stands.
func testContextRefs(t *testing.T, h Harness) {
	s, clk := openWith(t, h, store.TaskOptions{}, discard())
	ctx := context.Background()
	t0 := clk.Now()
	for _, c := range []store.Context{
		{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: t0},
		{Owner: "agent-kourier", ContextID: "c2", Ref: "sesn_2", LastActive: t0.Add(time.Minute)},
		{Owner: "someone-else", ContextID: "c1", Ref: "sesn_9", LastActive: t0},
	} {
		if err := s.PutContext(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutContext(ctx, store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_3", LastActive: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got := contexts(t, s)
	if len(got) != 3 || got["agent-kourier/c1"].Ref != "sesn_3" || !got["agent-kourier/c1"].LastActive.Equal(t0.Add(time.Hour)) ||
		got["agent-kourier/c2"].Ref != "sesn_2" || got["someone-else/c1"].Ref != "sesn_9" {
		t.Fatalf("contexts = %+v", got)
	}
	// A touch for a ref the context no longer has is dropped, and one owner's touch leaves another's context alone.
	if err := s.TouchContextRef(ctx, "agent-kourier", "c1", "sesn_1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchContextRef(ctx, "agent-kourier", "c2", "sesn_2", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchContextRef(ctx, "agent-kourier", "c1", "sesn_9", t0.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got = contexts(t, s)
	if !got["agent-kourier/c1"].LastActive.Equal(t0.Add(time.Hour)) || !got["agent-kourier/c2"].LastActive.Equal(t0.Add(3*time.Hour)) ||
		!got["someone-else/c1"].LastActive.Equal(t0) {
		t.Fatalf("after touches: %+v", got)
	}
}

func testForgetContext(t *testing.T, h Harness) {
	s, clk := openWith(t, h, store.TaskOptions{}, discard())
	ctx := context.Background()
	start := clk.Now()
	clk.Advance(time.Second)
	for _, c := range []store.Context{{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1"}, {Owner: "agent-kourier", ContextID: "c2", Ref: "sesn_2"}} {
		c.LastActive = clk.Now()
		if err := s.PutContext(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, tk := range []*a2a.Task{task("c1-done", "c1", a2a.TaskStateCompleted, nil), task("c1-paused", "c1", a2a.TaskStateInputRequired, nil), task("c2-done", "c2", a2a.TaskStateCompleted, nil)} {
		mustCreate(t, s, tk)
	}
	if _, err := s.Create(other, task("other-c1", "c1", a2a.TaskStateCompleted, nil)); err != nil {
		t.Fatal(err)
	}
	cutoff := clk.Now().Add(time.Nanosecond)
	clk.Advance(time.Minute)
	mustCreate(t, s, task("c1-after", "c1", a2a.TaskStateSubmitted, nil)) // a turn that started after the archive

	// A context whose ref was replaced keeps its new ref, and the tasks since.
	if err := s.ForgetContext(ctx, "agent-kourier", "c2", "sesn_old", start); err != nil {
		t.Fatal(err)
	}
	if got := contexts(t, s); got["agent-kourier/c2"].Ref != "sesn_2" {
		t.Fatalf("forgetting a replaced ref removed the context's current one: %+v", got)
	}

	if err := s.ForgetContext(ctx, "agent-kourier", "c1", "sesn_1", cutoff); err != nil {
		t.Fatal(err)
	}
	if got := contexts(t, s); len(got) != 1 || got["agent-kourier/c2"].Ref != "sesn_2" {
		t.Fatalf("contexts = %+v, want c2's alone", got)
	}
	for id, kept := range map[string]bool{"c1-done": false, "c1-paused": false, "c1-after": true, "c2-done": true} {
		_, err := s.Get(kourier, a2a.TaskID(id))
		if kept != (err == nil) {
			t.Errorf("%s: %v, want kept %v", id, err, kept)
		}
	}
	if _, err := s.Get(other, "other-c1"); err != nil {
		t.Errorf("another owner's task of the context: %v, want kept", err)
	}
	if got, _ := s.MessageTask(kourier, "msg-c1-done"); got != nil {
		t.Error("the message of a forgotten task is kept")
	}
	if got, _ := s.MessageTask(kourier, "msg-c2-done"); got == nil {
		t.Error("the message of another context's task is gone")
	}
	if got, _ := s.MessageTask(other, "msg-other-c1"); got == nil {
		t.Error("the message of another owner's task is gone")
	}
}

func testDeleteIdleContexts(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	// Idle: a settled task and a question nobody answered.
	mustCreate(t, s, task("idle-done", "idle", a2a.TaskStateCompleted, nil))
	mustCreate(t, s, task("idle-paused", "idle", a2a.TaskStateInputRequired, nil))
	// Idle by its last update, but a turn is running in it.
	mustCreate(t, s, task("busy-working", "busy", a2a.TaskStateWorking, nil))
	clk.Advance(time.Hour)
	// Another caller's context of the same ID, active later.
	if _, err := s.Create(other, task("other-done", "idle", a2a.TaskStateCompleted, nil)); err != nil {
		t.Fatal(err)
	}
	// Active: its first task is old, its last update is not.
	mustCreate(t, s, task("active-done", "active", a2a.TaskStateCompleted, nil))
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
	if _, ok := contexts(t, s)["agent-kourier/idle"]; ok {
		t.Error("the idle context is kept")
	}
	for _, id := range []string{"idle-done", "idle-paused"} {
		if _, err := s.Get(kourier, a2a.TaskID(id)); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("%s: %v, want deleted", id, err)
		}
		if got, err := s.MessageTask(kourier, "msg-"+id); err != nil || got != nil {
			t.Errorf("message of %s: %+v, %v; want deleted", id, got, err)
		}
	}
	for id, caller := range map[string]context.Context{"busy-working": kourier, "other-done": other, "active-done": kourier} {
		if _, err := s.Get(caller, a2a.TaskID(id)); err != nil {
			t.Errorf("%s: %v, want kept", id, err)
		}
	}
	// The context's next message starts it afresh, with its own activity.
	mustCreate(t, s, task("idle-again", "idle", a2a.TaskStateCompleted, nil))
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-90*time.Minute)); err != nil || n != 0 {
		t.Fatalf("a second DeleteIdleContexts = %d, %v; want 0", n, err)
	}
}

// ReleaseContext runs in the transaction that deletes an idle context, with the context as it was, and its error rolls
// the deletion back.
func testReleaseContext(t *testing.T, h Harness) {
	var released []store.Context
	fail := false
	release := func(ctx context.Context, q store.Querier, c store.Context) error {
		// It sees the deletion of its own transaction: the context's row is gone, its tasks not yet.
		var rows, tasks int
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM contexts WHERE owner = ? AND context_id = ?`, c.Owner, c.ContextID).Scan(&rows); err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM a2a_tasks WHERE owner = ? AND context_id = ?`, c.Owner, c.ContextID).Scan(&tasks); err != nil {
			return err
		}
		if rows != 0 || tasks == 0 {
			t.Errorf("release of %s saw %d context rows and %d tasks, want the row gone and the tasks there", c.ContextID, rows, tasks)
		}
		released = append(released, c)
		if fail {
			return errFenced
		}
		return nil
	}
	s, clk := openWith(t, h, store.TaskOptions{ContextActivity: true, ReleaseContext: release}, discard())
	ctx := context.Background()
	if err := s.PutContext(ctx, store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateCompleted, nil))
	at := clk.Now()
	clk.Advance(time.Hour)

	fail = true
	if n, err := s.DeleteIdleContexts(ctx, clk.Now()); !errors.Is(err, errFenced) || n != 0 {
		t.Fatalf("DeleteIdleContexts with a failing release = %d, %v; want its error", n, err)
	}
	if _, ok := contexts(t, s)["agent-kourier/c1"]; !ok {
		t.Fatal("the failed release's context was deleted")
	}
	if _, err := s.Get(kourier, "t1"); err != nil {
		t.Fatalf("the failed release's task: %v, want kept", err)
	}
	fail = false
	if n, err := s.DeleteIdleContexts(ctx, clk.Now()); err != nil || n != 1 {
		t.Fatalf("DeleteIdleContexts = %d, %v; want 1", n, err)
	}
	want := store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: at}
	if len(released) != 2 || released[1] != want {
		t.Fatalf("released %+v, want twice %+v", released, want)
	}
}

// A message to a context marks it active before its turn writes anything, so the idle sweep keeps the context the
// message is for; a context only the touch knows is swept once it is idle in turn.
func testTouchContext(t *testing.T, h Harness) {
	s, clk := open(t, h)
	ctx := context.Background()
	mustCreate(t, s, task("old-done", "touched", a2a.TaskStateCompleted, nil))
	if _, err := s.Create(other, task("other-done", "touched", a2a.TaskStateCompleted, nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutContext(ctx, store.Context{Owner: "agent-kourier", ContextID: "touched", Ref: "sesn_1", LastActive: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	if err := s.TouchContext(kourier, "touched"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchContext(kourier, "new"); err != nil {
		t.Fatal(err)
	}
	if c := contexts(t, s)["agent-kourier/touched"]; c.Ref != "sesn_1" || !c.LastActive.Equal(clk.Now()) {
		t.Fatalf("the touched context = %+v, want active now with its ref kept", c)
	}
	// Only the caller's context of that ID is touched: the other caller's is idle and goes.
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("DeleteIdleContexts = %d, %v; want the other caller's context alone", n, err)
	}
	if _, err := s.Get(kourier, "old-done"); err != nil {
		t.Errorf("the touched context's task: %v, want kept", err)
	}
	if _, err := s.Get(other, "other-done"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("the other caller's idle task: %v, want deleted", err)
	}
	clk.Advance(2 * time.Hour)
	if n, err := s.DeleteIdleContexts(ctx, clk.Now().Add(-time.Hour)); err != nil || n != 2 {
		t.Fatalf("DeleteIdleContexts once both are idle = %d, %v; want 2", n, err)
	}
}

func testReopen(t *testing.T, h Harness) {
	clk := NewClock()
	loc := h.New(t)
	db := h.Open(t, loc, discard())
	s := store.NewStore(db, store.TaskOptions{Now: clk.Now})
	ctx := context.Background()
	mustCreate(t, s, task("t1", "c1", a2a.TaskStateInputRequired, nil))
	if err := s.PutContext(ctx, store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}

	again := store.NewStore(h.Open(t, loc, discard()), store.TaskOptions{Now: clk.Now})
	if got, err := again.Get(kourier, "t1"); err != nil || got.Task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("Get after reopen: %+v, %v", got, err)
	}
	if got, err := again.MessageTask(kourier, "msg-t1"); err != nil || got == nil || got.ID != "t1" {
		t.Fatalf("MessageTask after reopen: %+v %v", got, err)
	}
	if c := contexts(t, again)["agent-kourier/c1"]; c.Ref != "sesn_1" || !c.LastActive.Equal(clk.Now()) {
		t.Fatalf("the context after reopen: %+v", c)
	}
}

// Every write runs the fence in its transaction: with a fence that refuses, every write fails and changes nothing, the
// message IDs a task would record included. A leader's epoch check, on Postgres, relies on this.
func testEveryWriteIsFenced(t *testing.T, h Harness) {
	s, _ := open(t, h)
	ctx := context.Background()
	seed := task("t1", "c1", a2a.TaskStateWorking, nil)
	mustCreate(t, s, seed)
	var mu sync.Mutex
	calls := 0
	store.SetFenceForTest(s.DB, func(fenceCtx context.Context, q store.Querier) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		// The fence writes a marker through the write's transaction, which the refusal must roll back with the write:
		// a fence that ran on another connection (a reader, or a write of its own) would fail to write it or leave it
		// behind.
		if _, err := q.Exec(fenceCtx, `INSERT INTO contexts (owner, context_id, last_active) VALUES (?, ?, ?)`,
			"fence-marker", fmt.Sprintf("marker-%d", calls), int64(calls)); err != nil {
			t.Errorf("the fence's marker: %v", err)
		}
		return errFenced
	})
	defer store.SetFenceForTest(s.DB, nil)
	future := time.Now().Add(time.Hour)
	writes := map[string]func() error{
		"Create": func() error { _, err := s.Create(kourier, task("t2", "c2", a2a.TaskStateSubmitted, nil)); return err },
		"Update": func() error {
			answered := task("t1", "c1", a2a.TaskStateWorking, nil)
			answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("answer"))
			answer.ID = "answer-1"
			answered.History = append(answered.History, answer)
			_, err := s.Update(kourier, &taskstore.UpdateRequest{Task: answered, PrevVersion: 1})
			return err
		},
		"SettleInterrupted":   func() error { _, err := s.SettleInterrupted(ctx, interruptedText); return err },
		"DeleteSettledBefore": func() error { _, err := s.DeleteSettledBefore(ctx, future); return err },
		"DeleteIdleContexts":  func() error { _, err := s.DeleteIdleContexts(ctx, future); return err },
		"TouchContext":        func() error { return s.TouchContext(kourier, "c1") },
		"PutContext": func() error {
			return s.PutContext(ctx, store.Context{Owner: "agent-kourier", ContextID: "c1", Ref: "sesn_1", LastActive: future})
		},
		"TouchContextRef": func() error { return s.TouchContextRef(ctx, "agent-kourier", "c1", "", future) },
		"ForgetContext":   func() error { return s.ForgetContext(ctx, "agent-kourier", "c1", "", future) },
	}
	for name, write := range writes {
		mu.Lock()
		before := calls
		mu.Unlock()
		if err := write(); !errors.Is(err, errFenced) {
			t.Errorf("%s: %v, want the fence's error", name, err)
		}
		mu.Lock()
		if calls == before {
			t.Errorf("%s did not run the fence", name)
		}
		mu.Unlock()
	}
	store.SetFenceForTest(s.DB, nil)
	all := contexts(t, s)
	if len(all) != 1 || all["agent-kourier/c1"].Ref != "" {
		t.Fatalf("contexts = %+v: a fenced write, or the fence's marker, outlived the refusal", all)
	}
	got, err := s.Get(kourier, "t1")
	if err != nil || got.Version != 1 || got.Task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("a fenced write changed the store: %+v, %v", got, err)
	}
	if _, err := s.Get(kourier, "t2"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("a fenced Create stored its task: %v", err)
	}
	for _, id := range []string{"msg-t2", "answer-1"} {
		if got, err := s.MessageTask(kourier, id); err != nil || got != nil {
			t.Fatalf("a fenced write recorded message %s: %+v, %v", id, got, err)
		}
	}
}
