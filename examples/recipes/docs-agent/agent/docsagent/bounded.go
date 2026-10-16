package docsagent

import (
	"container/list"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/adk/v2/session"
)

// Neither the A2A server's in-memory task store nor ADK's in-memory session service ever forgets an entry, so a
// long-running agent would grow without end. These wrappers bound them. The libraries offer no eviction hook, so
// each wrapper adds its own bookkeeping around the library's store.

// Default bounds. A session holds a conversation's events, which include the pages its tool calls returned (up to
// 24 KiB each), so the session bound is the one that sets the memory.
const (
	defaultMaxSessions = 200
	defaultMaxTasks    = 400
)

// sessionKey identifies a session.
type sessionKey struct{ app, user, id string }

func keyOf(s session.Session) sessionKey {
	return sessionKey{app: s.AppName(), user: s.UserID(), id: s.ID()}
}

// boundedSessions is a session.Service that holds at most limit sessions. When a Create takes it over the bound it
// deletes the least recently used session; creating, reading and appending an event to a session each count as a
// use. A session in the middle of a turn is the most recently used, so it is forgotten only if limit other sessions
// were used since.
type boundedSessions struct {
	session.Service
	limit int

	mu    sync.Mutex
	order *list.List // of sessionKey, most recently used first
	index map[sessionKey]*list.Element
}

func newBoundedSessions(inner session.Service, limit int) *boundedSessions {
	return &boundedSessions{Service: inner, limit: limit, order: list.New(), index: make(map[sessionKey]*list.Element)}
}

// track puts k first in the recency order, adding it if it is new, and returns the keys over the bound, which are no
// longer tracked and which the caller must delete. The caller holds b.mu.
func (b *boundedSessions) track(k sessionKey) (evict []sessionKey) {
	if el, ok := b.index[k]; ok {
		b.order.MoveToFront(el)
	} else {
		b.index[k] = b.order.PushFront(k)
	}
	for b.order.Len() > b.limit {
		oldest := b.order.Back()
		old := oldest.Value.(sessionKey)
		b.order.Remove(oldest)
		delete(b.index, old)
		evict = append(evict, old)
	}
	return evict
}

// use marks a tracked session as the most recently used. A session that is not tracked is not added. The caller
// holds b.mu.
func (b *boundedSessions) use(k sessionKey) {
	if el, ok := b.index[k]; ok {
		b.order.MoveToFront(el)
	}
}

// Every method below holds b.mu from the call into the wrapped service until the bookkeeping is done. Without that,
// a Get could return a session that another goroutine's Create then evicted and deleted before the Get counted as a
// use, and a caller would be handed a session that is gone. The wrapped service is in memory, so the lock is short.

// Create creates the session, then deletes those over the bound. Every deletion is attempted, and their errors are
// joined.
func (b *boundedSessions) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	resp, err := b.Service.Create(ctx, req)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, k := range b.track(keyOf(resp.Session)) {
		if err := b.Service.Delete(ctx, &session.DeleteRequest{AppName: k.app, UserID: k.user, SessionID: k.id}); err != nil {
			errs = append(errs, fmt.Errorf("session %s: %w", k.id, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("forget the least recently used sessions: %w", err)
	}
	return resp, nil
}

// Get returns the session and counts as a use of it.
func (b *boundedSessions) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	resp, err := b.Service.Get(ctx, req)
	if err != nil {
		return nil, err
	}
	b.use(keyOf(resp.Session))
	return resp, nil
}

// AppendEvent counts as a use of the session.
func (b *boundedSessions) AppendEvent(ctx context.Context, cur session.Session, event *session.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.Service.AppendEvent(ctx, cur, event); err != nil {
		return err
	}
	b.use(keyOf(cur))
	return nil
}

// Delete deletes the session and stops tracking it.
func (b *boundedSessions) Delete(ctx context.Context, req *session.DeleteRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.Service.Delete(ctx, req); err != nil {
		return err
	}
	k := sessionKey{app: req.AppName, user: req.UserID, id: req.SessionID}
	if el, ok := b.index[k]; ok {
		b.order.Remove(el)
		delete(b.index, k)
	}
	return nil
}

// boundedTasks is a taskstore.Store that holds between limit/2 and limit tasks: the newest limit/2 created, always, and
// the ones before them until the next generation fills. It is two in-memory stores, current and previous. Create
// fills the current one, and when that holds limit/2 tasks the previous one is dropped whole, the current becomes the
// previous and a fresh one begins. That forgets tasks by age with no per-task bookkeeping, which the library's
// store, having no delete, could not do. A task is looked up in both. A task still being updated when its
// generation is dropped is lost, which takes limit/2 newer tasks created during one turn.
type boundedTasks struct {
	perGeneration int

	mu       sync.RWMutex
	current  *taskstore.InMemory
	previous *taskstore.InMemory // nil until the first rotation
	created  int                 // tasks in current
}

var _ taskstore.Store = (*boundedTasks)(nil)

func newBoundedTasks(limit int) *boundedTasks {
	return &boundedTasks{perGeneration: max(limit/2, 1), current: newTaskGeneration()}
}

// anonymousOwner owns the tasks of a caller that sets no user, which is every caller of this agent: it checks no
// token. a2asrv's own authenticator answers such a caller with an empty name, which the store's List refuses as
// unauthenticated, so Get would find a task that List never lists.
const anonymousOwner = "anonymous"

// taskOwner is the store's authenticator. A call outside the A2A transport has no call context and is refused.
func taskOwner(ctx context.Context) (string, error) {
	cc, ok := a2asrv.CallContextFrom(ctx)
	if !ok {
		return "", a2a.ErrUnauthenticated
	}
	if cc.User != nil && cc.User.Name != "" {
		return cc.User.Name, nil
	}
	return anonymousOwner, nil
}

// newTaskGeneration is the library's in-memory store, which owns a task by the caller that created it.
func newTaskGeneration() *taskstore.InMemory {
	return taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: taskOwner})
}

func (b *boundedTasks) generations() (current, previous *taskstore.InMemory) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.current, b.previous
}

// Create stores the task in the current generation, starting a new one first if it is full.
func (b *boundedTasks) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.previous != nil {
		if _, err := b.previous.Get(ctx, task.ID); err == nil {
			return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
		}
	}
	if b.created >= b.perGeneration {
		b.previous, b.current, b.created = b.current, newTaskGeneration(), 0
	}
	v, err := b.current.Create(ctx, task)
	if err == nil {
		b.created++
	}
	return v, err
}

// Update updates the task in the generation that holds it.
func (b *boundedTasks) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	current, previous := b.generations()
	v, err := current.Update(ctx, req)
	if previous != nil && errors.Is(err, a2a.ErrTaskNotFound) {
		return previous.Update(ctx, req)
	}
	return v, err
}

// Get returns the task from the generation that holds it.
func (b *boundedTasks) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	current, previous := b.generations()
	t, err := current.Get(ctx, id)
	if previous != nil && errors.Is(err, a2a.ErrTaskNotFound) {
		return previous.Get(ctx, id)
	}
	return t, err
}

// Page size limits of tasks/list, as the library's store applies them.
const (
	defaultListPage = 50
	maxListPage     = 100
)

// List returns the caller's tasks that match the request, newest generation first, from both generations, in pages
// of the requested size (default 50, at most 100) that a token continues. Agent Kourier lists a context's tasks,
// without a page size or a history length, to find the task that holds a turn's message before it sends the turn
// again; each task comes with its history (the library's default is the newest 100 messages).
//
// A token names the last task of the page before it. A walk that goes on after that task has been forgotten ends,
// since everything after it in the order is older and was forgotten with it. A task updated during a walk may be
// listed twice or not at all, as with the library's own store; Agent Kourier counts a task once.
func (b *boundedTasks) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	// Agent Kourier always names the context (internal/agent/find.go). Refusing a list without one keeps a caller
	// from listing the questions of every thread: it can list only the context it already knows.
	if req.ContextID == "" {
		return nil, fmt.Errorf("a contextId is required to list tasks: %w", a2a.ErrInvalidRequest)
	}
	size := req.PageSize
	switch {
	case size == 0:
		size = defaultListPage
	case size < 1 || size > maxListPage:
		return nil, fmt.Errorf("page size must be between 1 and %d inclusive, got %d: %w", maxListPage, size, a2a.ErrInvalidRequest)
	}
	after, err := decodeListToken(req.PageToken)
	if err != nil {
		return nil, err
	}
	all, err := b.matching(ctx, req)
	if err != nil {
		return nil, err
	}
	start := 0
	if after != "" {
		start = len(all) // the task the token names is gone: nothing older is left
		for i, t := range all {
			if t.ID == after {
				start = i + 1
				break
			}
		}
	}
	end := min(start+size, len(all))
	resp := &a2a.ListTasksResponse{Tasks: all[start:end], TotalSize: len(all), PageSize: size}
	if end < len(all) {
		resp.NextPageToken = base64.URLEncoding.EncodeToString([]byte(all[end-1].ID))
	}
	return resp, nil
}

func decodeListToken(token string) (a2a.TaskID, error) {
	if token == "" {
		return "", nil
	}
	id, err := base64.URLEncoding.DecodeString(token)
	if err != nil || len(id) == 0 {
		return "", a2a.ErrParseError
	}
	return a2a.TaskID(id), nil
}

// matching returns every task of the caller that the request's filters select, the current generation's first, each
// in the order the library lists them (the most recently updated first).
func (b *boundedTasks) matching(ctx context.Context, req *a2a.ListTasksRequest) ([]*a2a.Task, error) {
	current, previous := b.generations()
	var all []*a2a.Task
	for _, gen := range []*taskstore.InMemory{current, previous} {
		if gen == nil {
			continue
		}
		page := &a2a.ListTasksRequest{
			Tenant: req.Tenant, ContextID: req.ContextID, Status: req.Status, StatusTimestampAfter: req.StatusTimestampAfter,
			HistoryLength: req.HistoryLength, IncludeArtifacts: req.IncludeArtifacts, PageSize: maxListPage,
		}
		for {
			resp, err := gen.List(ctx, page)
			if err != nil {
				return nil, err
			}
			all = append(all, resp.Tasks...)
			if resp.NextPageToken == "" {
				break
			}
			page.PageToken = resp.NextPageToken
		}
	}
	return all, nil
}
