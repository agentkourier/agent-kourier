package docsagent

import (
	"context"
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
)

func newSession(t *testing.T, svc session.Service, id string) session.Session {
	t.Helper()
	resp, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "u", SessionID: id})
	if err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	return resp.Session
}

// ids lists the sessions the service holds, without counting as a use of any of them.
func ids(t *testing.T, svc session.Service) []string {
	t.Helper()
	resp, err := svc.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range resp.Sessions {
		out = append(out, s.ID())
	}
	slices.Sort(out)
	return out
}

func wantSessions(t *testing.T, svc session.Service, want ...string) {
	t.Helper()
	if got := ids(t, svc); !slices.Equal(got, want) {
		t.Errorf("sessions held = %v, want %v", got, want)
	}
}

func TestBoundedSessionsForgetTheLeastRecentlyUsed(t *testing.T) {
	svc := newBoundedSessions(session.InMemoryService(), 3)
	for _, id := range []string{"a", "b", "c"} {
		newSession(t, svc, id)
	}
	wantSessions(t, svc, "a", "b", "c")
	// Reading "a" makes "b" the least recently used.
	if _, err := svc.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "u", SessionID: "a"}); err != nil {
		t.Fatal(err)
	}
	newSession(t, svc, "d")
	wantSessions(t, svc, "a", "c", "d")
	// A forgotten session is not found, and the executor then opens an empty one under the same ID.
	_, err := svc.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "u", SessionID: "b"})
	if !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get of a forgotten session = %v, want session.ErrNotFound", err)
	}
	newSession(t, svc, "b")
	wantSessions(t, svc, "a", "b", "d") // "c" was the least recently used
}

func TestBoundedSessionsAppendEventKeepsASessionInUse(t *testing.T) {
	svc := newBoundedSessions(session.InMemoryService(), 2)
	a := newSession(t, svc, "a")
	newSession(t, svc, "b")
	if err := svc.AppendEvent(t.Context(), a, session.NewEvent(t.Context(), "inv")); err != nil { // a is now the newest
		t.Fatal(err)
	}
	newSession(t, svc, "c") // forgets b
	wantSessions(t, svc, "a", "c")
}

func TestBoundedSessionsDeleteFreesASlot(t *testing.T) {
	svc := newBoundedSessions(session.InMemoryService(), 2)
	newSession(t, svc, "a")
	newSession(t, svc, "b")
	if err := svc.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "u", SessionID: "a"}); err != nil {
		t.Fatal(err)
	}
	newSession(t, svc, "c")
	wantSessions(t, svc, "b", "c")
}

func TestBoundedSessionsUnderConcurrency(t *testing.T) {
	svc := newBoundedSessions(session.InMemoryService(), 10)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				id := fmt.Sprintf("g%d-%d", g, i)
				if _, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "u", SessionID: id}); err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				_, _ = svc.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "u", SessionID: id})
			}
		}()
	}
	wg.Wait()
	if n := len(ids(t, svc)); n != 10 {
		t.Errorf("the service holds %d sessions after 400 creates, want the bound of 10", n)
	}
}

// callCtx is a context as the A2A transport makes one, which the task store reads the caller from.
func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, _ := a2asrv.NewCallContext(t.Context(), nil)
	return ctx
}

func task(id string) *a2a.Task {
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
}

func hasTask(t *testing.T, s taskstore.Store, id string) bool {
	t.Helper()
	_, err := s.Get(callCtx(t), a2a.TaskID(id))
	return err == nil
}

func TestBoundedTasksForgetTheOldest(t *testing.T) {
	s := newBoundedTasks(4) // two tasks per generation
	for i := range 10 {
		if _, err := s.Create(callCtx(t), task(fmt.Sprintf("t%d", i))); err != nil {
			t.Fatal(err)
		}
		held := 0
		for j := 0; j <= i; j++ {
			if hasTask(t, s, fmt.Sprintf("t%d", j)) {
				held++
			}
		}
		if held > 4 {
			t.Fatalf("after %d creates the store holds %d tasks, want at most 4", i+1, held)
		}
		// The newest half of the bound is always there.
		for _, j := range []int{i, i - 1} {
			if j >= 0 && !hasTask(t, s, fmt.Sprintf("t%d", j)) {
				t.Errorf("after %d creates, t%d was already forgotten", i+1, j)
			}
		}
	}
	if hasTask(t, s, "t0") || hasTask(t, s, "t5") {
		t.Error("old tasks are still held")
	}
}

func TestBoundedTasksUpdateReachesTheOlderGeneration(t *testing.T) {
	s := newBoundedTasks(4)
	v, err := s.Create(callCtx(t), task("old"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"x1", "x2"} { // fills the generation; the next create starts a new one
		if _, err := s.Create(callCtx(t), task(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(callCtx(t), task("new")); err != nil {
		t.Fatal(err)
	}
	done := task("old")
	done.Status.State = a2a.TaskStateCompleted
	if _, err := s.Update(callCtx(t), &taskstore.UpdateRequest{Task: done, PrevVersion: v}); err != nil {
		t.Fatalf("Update of a task in the older generation: %v", err)
	}
	got, err := s.Get(callCtx(t), "old")
	if err != nil || got.Task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("Get(old) = %+v, %v; want the completed task", got, err)
	}
	if _, err := s.Update(callCtx(t), &taskstore.UpdateRequest{Task: task("never-created")}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("Update of an unknown task = %v, want ErrTaskNotFound", err)
	}
}

func TestBoundedTasksRefuseADuplicateAcrossGenerations(t *testing.T) {
	s := newBoundedTasks(4)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.Create(callCtx(t), task(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(callCtx(t), task("a")); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Errorf("Create of a task held in the older generation = %v, want ErrTaskAlreadyExists", err)
	}
}

// taskIn is a task of a context, with a message in its history, as the A2A server stores a turn.
func taskIn(id, contextID string) *a2a.Task {
	t := task(id)
	t.ContextID = contextID
	t.History = []*a2a.Message{{ID: "m-" + id}}
	return t
}

func listIDs(t *testing.T, s *boundedTasks, req *a2a.ListTasksRequest) (ids []string, next string) {
	t.Helper()
	resp, err := s.List(callCtx(t), req)
	if err != nil {
		t.Fatalf("List(%+v): %v", req, err)
	}
	for _, task := range resp.Tasks {
		ids = append(ids, string(task.ID))
	}
	return ids, resp.NextPageToken
}

// TestBoundedTasksListAcrossGenerations pins what Agent Kourier's lookup needs: the tasks of one context from both
// generations, with their history, paged by token, none twice and none missed.
func TestBoundedTasksListAcrossGenerations(t *testing.T) {
	s := newBoundedTasks(8) // four tasks per generation
	for i := range 6 {      // t0..t3 fill one generation, t4 and t5 start the next
		ctx := "ctx-a"
		if i%2 == 1 {
			ctx = "ctx-b"
		}
		if _, err := s.Create(callCtx(t), taskIn(fmt.Sprintf("t%d", i), ctx)); err != nil {
			t.Fatal(err)
		}
	}
	got, next := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "ctx-a"})
	slices.Sort(got)
	if want := []string{"t0", "t2", "t4"}; !slices.Equal(got, want) || next != "" {
		t.Errorf("tasks of ctx-a = %v (next %q), want %v from both generations", got, next, want)
	}
	resp, err := s.List(callCtx(t), &a2a.ListTasksRequest{ContextID: "ctx-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range resp.Tasks {
		if len(task.History) != 1 || task.History[0].ID != "m-"+string(task.ID) {
			t.Errorf("task %s lists history %v, want its message: the lookup finds a turn by it", task.ID, task.History)
		}
	}
	if resp.TotalSize != 3 || resp.PageSize != 50 {
		t.Errorf("totalSize = %d, pageSize = %d; want 3 and the default 50", resp.TotalSize, resp.PageSize)
	}

	// The same three, one to a page.
	var all []string
	req := &a2a.ListTasksRequest{ContextID: "ctx-a", PageSize: 1}
	for range 10 {
		page, tok := listIDs(t, s, req)
		all = append(all, page...)
		if tok == "" {
			break
		}
		req.PageToken = tok
	}
	slices.Sort(all)
	if want := []string{"t0", "t2", "t4"}; !slices.Equal(all, want) {
		t.Errorf("paging one at a time gave %v, want each of %v once", all, want)
	}
	if got, _ := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "none"}); len(got) != 0 {
		t.Errorf("tasks of an unknown context = %v, want none", got)
	}
	if got, _ := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "ctx-a", Status: a2a.TaskStateCompleted}); len(got) != 0 {
		t.Errorf("completed tasks = %v, want none: the status filter applies", got)
	}
}

func TestBoundedTasksListNewestFirst(t *testing.T) {
	s := newBoundedTasks(4) // two per generation
	for _, id := range []string{"t0", "t1", "t2"} {
		if _, err := s.Create(callCtx(t), taskIn(id, "c")); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "c"})
	if got[0] != "t2" {
		t.Errorf("order = %v, want the newest generation first", got)
	}
}

func TestBoundedTasksListRefusals(t *testing.T) {
	s := newBoundedTasks(4)
	for name, req := range map[string]*a2a.ListTasksRequest{
		"a page size of 101":                     {ContextID: "c", PageSize: 101},
		"a negative size":                        {ContextID: "c", PageSize: -1},
		"no contextId":                           {},
		"no contextId, a page size and a status": {PageSize: 10, Status: a2a.TaskStateSubmitted},
	} {
		if _, err := s.List(callCtx(t), req); !errors.Is(err, a2a.ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := s.List(callCtx(t), &a2a.ListTasksRequest{ContextID: "c", PageToken: "!!"}); !errors.Is(err, a2a.ErrParseError) {
		t.Errorf("a bad page token: %v, want ErrParseError", err)
	}
	if _, err := s.List(t.Context(), &a2a.ListTasksRequest{ContextID: "c"}); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("a call outside the transport: %v, want ErrUnauthenticated", err)
	}
}

func TestBoundedTasksListPassesTheHistoryLengthThrough(t *testing.T) {
	s := newBoundedTasks(4)
	if _, err := s.Create(callCtx(t), taskIn("t0", "c")); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		length *int
		want   int
	}{
		"unset: the library's default of 100": {nil, 1},
		"none":                                {new(0), 0},
		"one":                                 {new(1), 1},
	} {
		resp, err := s.List(callCtx(t), &a2a.ListTasksRequest{ContextID: "c", HistoryLength: tt.length})
		if err != nil || len(resp.Tasks) != 1 {
			t.Fatalf("%s: List = %+v, %v", name, resp, err)
		}
		if got := len(resp.Tasks[0].History); got != tt.want {
			t.Errorf("%s: %d messages of history, want %d", name, got, tt.want)
		}
	}
}

// TestBoundedTasksListEndsAtAForgottenToken pins what a walk does when the task its token names has been forgotten
// since the last page: everything after it was older and is gone too, so the walk ends and does not start over.
func TestBoundedTasksListEndsAtAForgottenToken(t *testing.T) {
	s := newBoundedTasks(4) // two per generation
	for _, id := range []string{"t0", "t1"} {
		if _, err := s.Create(callCtx(t), taskIn(id, "c")); err != nil {
			t.Fatal(err)
		}
	}
	first, next := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "c", PageSize: 1})
	if len(first) != 1 || next == "" {
		t.Fatalf("first page = %v, next %q; want one task and a token", first, next)
	}
	for _, id := range []string{"t2", "t3", "t4", "t5"} { // two rotations: the first page's task is dropped
		if _, err := s.Create(callCtx(t), taskIn(id, "c")); err != nil {
			t.Fatal(err)
		}
	}
	if hasTask(t, s, first[0]) {
		t.Fatalf("%s is still held; the test needs it forgotten", first[0])
	}
	got, after := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "c", PageSize: 10, PageToken: next})
	if len(got) != 0 || after != "" {
		t.Errorf("page after a forgotten token = %v (next %q), want an empty end of the walk", got, after)
	}
}

// TestBoundedTasksGetAndListAgreeOnTheOwner pins that the anonymous caller, which is every caller of this agent,
// sees what it created in both calls, and that a named caller does not see its tasks.
func TestBoundedTasksGetAndListAgreeOnTheOwner(t *testing.T) {
	s := newBoundedTasks(4)
	if _, err := s.Create(callCtx(t), taskIn("t0", "c")); err != nil {
		t.Fatal(err)
	}
	if !hasTask(t, s, "t0") {
		t.Error("Get does not find the anonymous caller's task")
	}
	if got, _ := listIDs(t, s, &a2a.ListTasksRequest{ContextID: "c"}); !slices.Equal(got, []string{"t0"}) {
		t.Errorf("List = %v, want t0", got)
	}
	ctx, cc := a2asrv.NewCallContext(t.Context(), nil)
	cc.User = a2asrv.NewAuthenticatedUser("bob", nil)
	if _, err := s.Get(ctx, "t0"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("another user's Get = %v, want ErrTaskNotFound", err)
	}
	resp, err := s.List(ctx, &a2a.ListTasksRequest{ContextID: "c"})
	if err != nil || len(resp.Tasks) != 0 {
		t.Errorf("another user's List = %+v, %v; want no tasks", resp, err)
	}
}

// pausedGet is a session service whose Get of one session stops, after it has found the session, until released.
type pausedGet struct {
	session.Service
	id      string
	found   chan struct{}
	release chan struct{}
}

func (p *pausedGet) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	resp, err := p.Service.Get(ctx, req)
	if err == nil && req.SessionID == p.id {
		close(p.found)
		<-p.release
	}
	return resp, err
}

// TestBoundedSessionsAGetThatSucceedsCountsAsAUse pins that a Create cannot evict a session between the moment a Get
// found it and the moment the Get counted as a use. The Get is held after it has found "a", and a Create that would
// evict "a" starts meanwhile: it has to wait for the Get, which makes "b" the one forgotten. (The wait is a fixed
// 50ms before the Get is released; the check holds whatever the timing, and without the lock a Create that has
// finished by then fails it.)
func TestBoundedSessionsAGetThatSucceedsCountsAsAUse(t *testing.T) {
	inner := &pausedGet{Service: session.InMemoryService(), id: "a", found: make(chan struct{}), release: make(chan struct{})}
	svc := newBoundedSessions(inner, 2)
	newSession(t, svc, "a")
	newSession(t, svc, "b")

	var wg sync.WaitGroup
	var getErr error
	wg.Go(func() {
		_, getErr = svc.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "u", SessionID: "a"})
	})
	<-inner.found
	wg.Go(func() { newSession(t, svc, "c") })
	time.Sleep(50 * time.Millisecond)
	close(inner.release)
	wg.Wait()

	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	wantSessions(t, svc, "a", "c") // the Get was the later use, so "b" went
}
