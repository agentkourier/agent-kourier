package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

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

// fakeBackend is a Managed Agents session service in memory. A stream receives only the events published after it
// opened, as the real one does. respond scripts the agent: it returns the events that follow an outbound event.
type fakeBackend struct {
	mu         sync.Mutex
	sessions   []string
	records    map[string]*fakeSession
	archived   []string
	archiveErr error // returned, once, by the next Archive
	sendErr    error // returned by every Send, which then sends nothing
	sent       []sentEvent
	streams    map[string][]*fakeStream
	respond    func(session string, out Outbound) []Event
	seq        int
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
