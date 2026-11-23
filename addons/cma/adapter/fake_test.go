package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// serve serves exec on st as the kit's runtime serves a backend on the leader (Addon.Handler, behind telemetry.Extract),
// with the caller check off, so that every request runs as the adapter's one caller, and returns an A2A client of it.
// tel counts the turns, and may be nil. The server stops when the test ends.
func serve(t *testing.T, exec *Executor, st *store.Store, tel *telemetry.Telemetry) *a2aclient.Client {
	t.Helper()
	return dial(t, serveURL(t, exec, st, tel), nil)
}

// serveURL serves exec as serve does and returns the endpoint's URL.
func serveURL(t *testing.T, exec *Executor, st *store.Store, tel *telemetry.Telemetry) string {
	t.Helper()
	return serveSeen(t, exec, st, tel, nil)
}

// serveSeen serves exec as serveURL does, and hands the server's context of each request to requests, when it is not
// nil, before it serves it.
func serveSeen(t *testing.T, exec *Executor, st *store.Store, tel *telemetry.Telemetry, requests chan<- context.Context) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	url := srv.URL + runtime.A2APath
	check := &auth.Check{Verifier: auth.Open{}, Log: slog.New(slog.DiscardHandler)}
	endpoint := telemetry.Extract(Addon("9.9.9-test", nil).Handler(exec, st, check, url, tel))
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			requests <- r.Context()
		}
		endpoint.ServeHTTP(w, r)
	})
	return url
}

// dial is an A2A client of the endpoint at url, over transport (nil is a plain one).
func dial(t *testing.T, url string, transport http.RoundTripper) *a2aclient.Client {
	t.Helper()
	if transport == nil {
		transport = &http.Transport{}
	}
	card := &a2a.AgentCard{Name: "test", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)}}
	c, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// kourier is a context whose call runs as the adapter's one caller, for a test that reads the store itself.
func kourier(ctx context.Context) context.Context {
	ctx, callCtx := a2asrv.NewCallContext(ctx, nil)
	callCtx.User = a2asrv.NewAuthenticatedUser(auth.Caller, nil)
	return ctx
}

// newStore is a SQLite store in the test's directory, stamped by now (nil is time.Now), closed when the test ends.
func newStore(t *testing.T, now func() time.Time) *store.Store {
	t.Helper()
	return openStore(t, filepath.Join(t.TempDir(), "cma.db"), now)
}

// openStore opens the SQLite store at path; the test closes it when it ends, or before to reopen the file.
func openStore(t *testing.T, path string, now func() time.Time) *store.Store {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), path, store.Options{Now: now})
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// corrupt overwrites the stored record of task id with one that does not decode, as a damaged or foreign write leaves it.
func corrupt(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if err := s.Write(context.Background(), func(q kitstore.Querier) error {
		_, err := q.Exec(context.Background(), `UPDATE a2a_tasks SET payload = ? WHERE id = ?`, []byte(`[1]`), id)
		return err
	}); err != nil {
		t.Fatalf("corrupt %s: %v", id, err)
	}
}

// fakeBackend is a Managed Agents session service in memory. A stream receives only the events published after it
// opened, as the real one does. respond scripts the agent: it returns the events that follow an outbound event.
type fakeBackend struct {
	mu         sync.Mutex
	sessions   []string
	records    map[string]*fakeSession
	archived   []string
	archiveErr error // returned, once, by the next Archive
	createErr  error // returned, once, by the next CreateSession
	sendErr    error // returned by every Send, which then sends nothing
	sent       []sentEvent
	streams    map[string][]*fakeStream
	respond    func(session string, out Outbound) []Event
	seq        int
	// beforeCreate and beforeSend, when set, run before the call does anything, outside the fake's lock: a test blocks
	// in them to hold a call open.
	beforeCreate func(contextID string)
	beforeSend   func(session string, events []Outbound)
}

// fakeSession is what the fake keeps of a session for Sessions and Archive.
type fakeSession struct {
	contextID  string
	lastActive time.Time
	archived   bool
}

type sentEvent struct {
	session string
	out     Outbound
}

func newFake(respond func(session string, out Outbound) []Event) *fakeBackend {
	return &fakeBackend{streams: map[string][]*fakeStream{}, records: map[string]*fakeSession{}, respond: respond}
}

func (f *fakeBackend) CreateSession(_ context.Context, _ string, metadata map[string]string) (string, error) {
	if f.beforeCreate != nil {
		f.beforeCreate(metadata[contextMetadataKey])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createErr; err != nil {
		f.createErr = nil
		return "", err
	}
	id := fmt.Sprintf("sesn_%d", len(f.sessions)+1)
	f.sessions = append(f.sessions, id)
	f.records[id] = &fakeSession{contextID: metadata[contextMetadataKey]}
	return id, nil
}

// seed adds a session that exists before the adapter starts.
func (f *fakeBackend) seed(id, contextID string, lastActive time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[id] = &fakeSession{contextID: contextID, lastActive: lastActive}
}

func (f *fakeBackend) Sessions(_ context.Context) ([]SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []SessionInfo
	for id, r := range f.records {
		if !r.archived && r.contextID != "" {
			out = append(out, SessionInfo{ID: id, ContextID: r.contextID, LastActive: r.lastActive})
		}
	}
	return out, nil
}

func (f *fakeBackend) Archive(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.archiveErr; err != nil {
		f.archiveErr = nil
		return err
	}
	if r, ok := f.records[session]; ok {
		r.archived = true
	}
	f.archived = append(f.archived, session)
	return nil
}

func (f *fakeBackend) archivedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.archived...)
}

func (f *fakeBackend) Stream(ctx context.Context, session string) (EventStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeStream{ch: make(chan Event, 64), done: make(chan struct{})}
	f.streams[session] = append(f.streams[session], s)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

func (f *fakeBackend) Send(_ context.Context, session string, events ...Outbound) error {
	if f.beforeSend != nil {
		f.beforeSend(session, events)
	}
	f.mu.Lock()
	if f.sendErr != nil {
		f.mu.Unlock()
		return f.sendErr
	}
	var follow []Event
	for _, ev := range events {
		f.sent = append(f.sent, sentEvent{session: session, out: ev})
		if f.respond != nil {
			follow = append(follow, f.respond(session, ev)...)
		}
	}
	f.mu.Unlock()
	f.publish(session, follow...)
	return nil
}

// publish delivers events to every open stream of the session, giving each an ID if it has none.
func (f *fakeBackend) publish(session string, events ...Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range events {
		if ev.ID == "" {
			f.seq++
			ev.ID = fmt.Sprintf("sevt_%d", f.seq)
		}
		for _, s := range f.streams[session] {
			s.push(ev)
		}
	}
}

func (f *fakeBackend) sentEvents() []sentEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentEvent(nil), f.sent...)
}

func (f *fakeBackend) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

type fakeStream struct {
	ch     chan Event
	done   chan struct{}
	once   sync.Once
	cur    Event
	closed bool
	mu     sync.Mutex
}

func (s *fakeStream) push(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.ch <- ev
}

func (s *fakeStream) Next() bool {
	select {
	case ev := <-s.ch:
		s.cur = ev
		return true
	case <-s.done:
		return false
	}
}

func (s *fakeStream) Current() Event { return s.cur }
func (s *fakeStream) Err() error     { return nil }
func (s *fakeStream) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

// Event builders for scripts.

func processed() *time.Time { t := time.Now(); return &t }

func running() Event { return Event{Type: evStatusRunning} }

func idle(reason string, ids ...string) Event {
	return Event{Type: evStatusIdle, StopReason: &StopReason{Type: reason, EventIDs: ids}}
}

func say(text string) Event {
	return Event{Type: evAgentMessage, Content: []ContentBlock{{Type: "text", Text: text}}}
}

func toolUse(id, name string, input map[string]any, permission string) Event {
	raw, _ := json.Marshal(input)
	return Event{ID: id, Type: evAgentToolUse, Name: name, Input: raw, EvaluatedPermission: permission}
}

func toolResult(useID, text string) Event {
	return Event{Type: evAgentToolResult, ToolUseID: useID, Content: []ContentBlock{{Type: "text", Text: text}}}
}
