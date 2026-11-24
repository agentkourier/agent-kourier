package runtime

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// fakeStore is the kit's task state in memory, owner-scoped as the real store is, with the message IDs it records and
// a lease that another replica may hold. It logs what the stop's order depends on: its close, after the events others
// log in it.
type fakeStore struct {
	*taskstore.InMemory

	mu        sync.Mutex
	messages  map[string]a2a.TaskID           // the task each message ID started or joined
	other     *store.Lease                    // a live lease another replica holds: TryLead fails and CurrentLeader returns it
	idle      []store.Context                 // what IdleContexts returns
	listed    func()                          // runs once IdleContexts has read its list, before it returns
	forgets   func()                          // runs as ForgetContext begins, before it removes the context
	forgotten []string                        // the contexts ForgetContext removed
	touched   []string                        // the contexts TouchContext marked active, in order
	contexts  []string                        // "touch <id>" and "forget <id>", in the order they ran
	deleted   int                             // what DeleteIdleContexts returns
	log       []string                        // the events in order: "close" and what the test adds
	settle    func(ctx context.Context) error // runs in SettleInterrupted: its error is the settlement's
	settled   store.Settlement                // what SettleInterrupted returns
	detached  bool                            // the writes ignore their context's end, as a store whose writes have begun would
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{InMemory: taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: owner}), messages: map[string]a2a.TaskID{}}
}

// owner is the call's caller, as the kit's store reads it.
func owner(ctx context.Context) (string, error) {
	if cc, ok := a2asrv.CallContextFrom(ctx); ok && cc.User != nil && cc.User.Authenticated && cc.User.Name != "" {
		return cc.User.Name, nil
	}
	return "", a2a.ErrUnauthenticated
}

func (s *fakeStore) record(id a2a.TaskID, msgs ...*a2a.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		if m != nil && m.ID != "" {
			s.messages[m.ID] = id
		}
	}
}

func (s *fakeStore) note(event string) {
	s.mu.Lock()
	s.log = append(s.log, event)
	s.mu.Unlock()
}

func (s *fakeStore) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

func (s *fakeStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if err := ctx.Err(); err != nil && !s.detached {
		return taskstore.TaskVersionMissing, err
	}
	v, err := s.InMemory.Create(ctx, task)
	if err == nil {
		s.record(task.ID, task.History...)
	}
	return v, err
}

func (s *fakeStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if err := ctx.Err(); err != nil && !s.detached {
		return taskstore.TaskVersionMissing, err
	}
	v, err := s.InMemory.Update(ctx, req)
	if err == nil {
		msg, _ := req.Event.(*a2a.Message)
		s.record(req.Task.ID, msg)
	}
	return v, err
}

func (s *fakeStore) MessageTask(ctx context.Context, messageID string) (*a2a.Task, error) {
	s.mu.Lock()
	id, ok := s.messages[messageID]
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	st, err := s.Get(ctx, id)
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil, nil // another caller's
	}
	if err != nil {
		return nil, err
	}
	return st.Task, nil
}

func (s *fakeStore) TouchContext(_ context.Context, contextID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, contextID)
	s.contexts = append(s.contexts, "touch "+contextID)
	return nil
}

func (s *fakeStore) touchedContexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.touched)
}

// contextEvents are the touches and forgets of contexts, in the order they ran.
func (s *fakeStore) contextEvents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.contexts)
}

func (s *fakeStore) SettleInterrupted(ctx context.Context, _ string) (store.Settlement, error) {
	if s.settle != nil {
		if err := s.settle(ctx); err != nil {
			return store.Settlement{}, err
		}
	}
	return s.settled, nil
}

func (s *fakeStore) DeleteSettledBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *fakeStore) DeleteIdleContexts(context.Context, time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted, nil
}

func (s *fakeStore) IdleContexts(context.Context, time.Time) ([]store.Context, error) {
	s.mu.Lock()
	idle := slices.Clone(s.idle)
	s.mu.Unlock()
	if s.listed != nil {
		s.listed()
	}
	return idle, nil
}

// StillIdle reports a listed context still idle until it is marked active: a touch moves its last activity.
func (s *fakeStore) StillIdle(_ context.Context, c store.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !slices.Contains(s.touched, c.ContextID), nil
}

func (s *fakeStore) ForgetContext(_ context.Context, _, contextID, _ string, _ time.Time) error {
	if s.forgets != nil {
		s.forgets()
	}
	s.mu.Lock()
	s.forgotten = append(s.forgotten, contextID)
	s.contexts = append(s.contexts, "forget "+contextID)
	s.mu.Unlock()
	return nil
}

func (s *fakeStore) TryLead(_ context.Context, holder, address string, _ time.Duration) (store.Lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.other != nil {
		return *s.other, false, nil
	}
	return store.Lease{Holder: holder, Address: address, Live: true}, true, nil
}

func (s *fakeStore) Renew(context.Context, time.Duration) error { return nil }
func (s *fakeStore) Release(context.Context) error              { s.note("release"); return nil }

func (s *fakeStore) CurrentLeader(context.Context) (store.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.other != nil {
		return *s.other, nil
	}
	return store.Lease{}, nil
}

func (s *fakeStore) Close() error { s.note("close"); return nil }

// fakeTask leaves a task in the store as the caller's.
func (s *fakeStore) fakeTask(t *testing.T, task *a2a.Task) {
	t.Helper()
	if _, err := s.InMemory.Create(asCaller(context.Background(), auth.Caller), task); err != nil {
		t.Fatal(err)
	}
	s.record(task.ID, task.History...)
}

// asCaller is ctx in an A2A call of the caller name.
func asCaller(ctx context.Context, name string) context.Context {
	ctx, call := a2asrv.NewCallContext(ctx, nil)
	call.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx
}

// scripted is a backend whose turns a test drives through the message's text: "pause" asks a question, "hold" runs
// until the turn's context ends and returns its error, "quiet" runs until then and returns nothing, "slow" waits for gate before its first event, "fail" ends failed with its status
// message, anything else completes. An answer to a paused task completes it; one whose text is "bad" fails the turn
// with a2a.ErrInvalidParams, as a backend that checks an answer only in its turn does.
type scripted struct {
	gate    chan struct{} // closed to let a "slow" turn go on
	started chan string   // told each turn's text as it starts, if set
	mu      sync.Mutex
	turns   int
	cleaned []a2a.TaskID // the executions Cleanup was told of
}

func (b *scripted) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		b.mu.Lock()
		b.turns++
		b.mu.Unlock()
		text := ""
		if len(ec.Message.Parts) > 0 && ec.Message.Parts[0] != nil {
			text = ec.Message.Parts[0].Text()
		}
		if b.started != nil {
			b.started <- text
		}
		if text == "slow" {
			select {
			case <-b.gate:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		if ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired && text == "bad" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		switch text {
		case "pause":
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("which?"))), nil)
			return
		case "hold":
			<-ctx.Done()
			yield(nil, ctx.Err())
			return
		case "quiet":
			<-ctx.Done()
			return
		case "fail":
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("timeout"))), nil)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

func (b *scripted) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (b *scripted) Cleanup(_ context.Context, ec *a2asrv.ExecutorContext, _ a2a.SendMessageResult, _ error) {
	b.mu.Lock()
	b.cleaned = append(b.cleaned, ec.TaskID)
	b.mu.Unlock()
}

func (b *scripted) turnCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turns
}

// validating is scripted with a MessageValidator that refuses a message "bad", new or an answer.
type validating struct{ *scripted }

func (validating) ValidateMessage(_ context.Context, msg *a2a.Message, _ *a2a.Task) error {
	if len(msg.Parts) == 1 && msg.Parts[0] != nil && msg.Parts[0].Text() == "bad" {
		return errors.New("the answer is bad")
	}
	return nil
}

// classifying is scripted with a TurnClassifier that names a failed turn by its status message.
type classifying struct{ *scripted }

func (classifying) FailedOutcome(status a2a.TaskStatus) telemetry.TurnOutcome {
	if status.Message != nil && len(status.Message.Parts) == 1 && status.Message.Parts[0].Text() == "timeout" {
		return "timeout"
	}
	return ""
}

// testAddon is an add-on named "kit" whose store is st and whose backend is b.
func testAddon(st *fakeStore, b Backend) Addon[*fakeStore] {
	return Addon[*fakeStore]{
		Name: "kit", Noun: "agent", Version: "1.2.3-test",
		Telemetry: telemetry.Spec{Calls: "backend", TurnsHelp: "Turns.", OperationHelp: "Calls.", AuthRefusedHelp: "Refusals.",
			ErrorTurns: []telemetry.TurnOutcome{"timeout"}},
		Card:  Card{Name: "kit-test", Description: "the runtime's test add-on"},
		Open:  func(context.Context, settings.Config, StoreOptions) (*fakeStore, error) { return st, nil },
		Build: func(context.Context, Env[*fakeStore]) (Backend, error) { return b, nil },
	}
}

func newTel(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(testAddon(nil, nil).TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// metricOf is the value of series in what tel serves, or "".
func metricOf(t *testing.T, tel *telemetry.Telemetry, series string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

// wantMetric waits up to five seconds for series to be want: a turn is counted when its execution returns.
func wantMetric(t *testing.T, tel *telemetry.Telemetry, series, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := metricOf(t, tel, series)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		got = metricOf(t, tel, series)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", series, got, want)
	}
}

// bearerIsCaller runs a request as the caller its bearer token names.
type bearerIsCaller struct{}

func (bearerIsCaller) Verify(r *http.Request) (string, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", auth.ErrMissing
	}
	return token, nil
}

// served is the handler of the add-on's endpoint (Addon.Handler) on an httptest server, and a client of it as caller.
type served struct {
	srv *httptest.Server
	tel *telemetry.Telemetry
}

func serveHandler(t *testing.T, st *fakeStore, b Backend) *served {
	t.Helper()
	tel := newTel(t)
	check := &auth.Check{Verifier: bearerIsCaller{}, Bearer: true, Log: slog.New(slog.DiscardHandler)}
	s := &served{srv: httptest.NewServer(testAddon(st, b).Handler(b, st, check, "http://kit.test/a2a", tel)), tel: tel}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *served) client(t *testing.T, caller string) *a2aclient.Client {
	t.Helper()
	return clientOf(t, s.srv.URL, caller)
}

func clientOf(t *testing.T, base, caller string) *a2aclient.Client {
	t.Helper()
	card := &a2a.AgentCard{Name: "test", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base+A2APath, a2a.TransportProtocolJSONRPC)}}
	c, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearer{caller}}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return (&http.Transport{DisableKeepAlives: true}).RoundTrip(r)
}

// message is a user message of text with the ID id, for the context and task given.
func message(id, text string, contextID string, taskID a2a.TaskID) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ID, m.ContextID, m.TaskID = id, contextID, taskID
	return m
}

// send sends msg streaming and returns its task as it stands once the stream ends.
func send(t *testing.T, c *a2aclient.Client, msg *a2a.Message) (*a2a.Task, error) {
	t.Helper()
	var id a2a.TaskID
	for ev, err := range c.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			return nil, err
		}
		id = taskOf(ev)
	}
	return c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: id})
}

func taskOf(ev a2a.Event) a2a.TaskID {
	switch v := ev.(type) {
	case *a2a.Task:
		return v.ID
	case *a2a.TaskStatusUpdateEvent:
		return v.TaskID
	case *a2a.TaskArtifactUpdateEvent:
		return v.TaskID
	}
	return ""
}
