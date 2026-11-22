package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/adk/v2/session"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// upkeepFixture is a store with, as of an hour ago, a settled turn in context "old" that has an ADK session; and, now,
// a settled turn in context "new".
func upkeepFixture(t *testing.T) (*store.Store, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	st, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "ax.db"), store.Options{Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := callerContext("binding")
	if _, err := st.Create(ctx, &a2a.Task{ID: "old", ContextID: "old", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sessions().Create(ctx, &session.CreateRequest{AppName: store.AppName, UserID: "binding", SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	if _, err := st.Create(ctx, &a2a.Task{ID: "new", ContextID: "new", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}); err != nil {
		t.Fatal(err)
	}
	return st, clk
}

func kept(t *testing.T, st *store.Store, id a2a.TaskID) bool {
	t.Helper()
	_, err := st.Get(callerContext("binding"), id)
	if err != nil && !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func hasSession(t *testing.T, st *store.Store, contextID string) bool {
	t.Helper()
	_, err := st.Sessions().Get(context.Background(), &session.GetRequest{AppName: store.AppName, UserID: "binding", SessionID: contextID})
	return err == nil
}

func TestUpkeepDeletesIdleContextsAndOldTasks(t *testing.T) {
	for _, tc := range []struct {
		name                string
		retention, idle     time.Duration
		oldTask, oldSession bool
		newTask             bool
	}{
		{"idle context goes with its session", 0, 30 * time.Minute, false, false, true},
		{"old settled task goes, its session stays", 30 * time.Minute, 0, false, true, true},
		{"both zero keep everything", 0, 0, true, true, true},
		{"nothing old enough", 2 * time.Hour, 2 * time.Hour, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, clk := upkeepFixture(t)
			(&Upkeep{Store: st, TaskRetention: tc.retention, SessionIdleTTL: tc.idle, Log: discard(), Now: clk.now}).Sweep(t.Context())
			if got := kept(t, st, "old"); got != tc.oldTask {
				t.Errorf("old task kept = %v, want %v", got, tc.oldTask)
			}
			if got := hasSession(t, st, "old"); got != tc.oldSession {
				t.Errorf("old session kept = %v, want %v", got, tc.oldSession)
			}
			if got := kept(t, st, "new"); got != tc.newTask {
				t.Errorf("new task kept = %v, want %v", got, tc.newTask)
			}
		})
	}
}

func TestUpkeepRunReturnsAtOnceWithNothingToDo(t *testing.T) {
	done := make(chan struct{})
	go func() { (&Upkeep{Log: discard()}).Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with both durations zero did not return")
	}
}
