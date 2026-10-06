package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestEndStaleTaskIsFenced: ending a task is a write of the leader like any other. A standby, and a leader that another
// has since replaced, change nothing and are told so; a listing is a read, and is not refused.
func TestEndStaleTaskIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	if _, err := a.Lead(t.Context(), "pod-a"); err != nil {
		t.Fatal(err)
	}
	k := oldSession(t, a, "1.1")
	if err := a.SetSessionTask(t.Context(), k, "", "task-1", store.TaskTurn{}); err != nil {
		t.Fatal(err)
	}
	stale := store.StaleTask{Key: k, Binding: "ns/b", ContextID: "c", TaskID: "task-1"}
	now := t0.Add(2 * time.Hour)

	if _, err := b.EndStaleTask(t.Context(), anyHorizon, stale, now); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("a standby's EndStaleTask = %v; want store.ErrDeposed", err)
	}
	if _, err := b.Lead(t.Context(), "pod-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.EndStaleTask(t.Context(), anyHorizon, stale, now); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("a deposed leader's EndStaleTask = %v; want store.ErrDeposed", err)
	}
	if got, err := a.ListStaleTasks(t.Context(), anyHorizon, 10); err != nil || len(got) != 1 {
		t.Errorf("a deposed leader's ListStaleTasks = %+v, %v; want the task: a read is not fenced, and nothing was changed", got, err)
	}
	got, err := b.EndStaleTask(t.Context(), anyHorizon, stale, now)
	if err != nil || !got.Ended {
		t.Errorf("the leader's EndStaleTask = %+v, %v; want the task ended", got, err)
	}
	if s, err := b.GetSession(t.Context(), k, 1000*time.Hour*24, t0); err != nil || s.TaskID != "" {
		t.Errorf("the session after = task %q, %v; want no task", s.TaskID, err)
	}
}
