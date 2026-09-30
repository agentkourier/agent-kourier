package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// SetSessionTask changes the task and the contextId and nothing else: least of all
// LastActivity, which a concurrent TouchSession may just have moved.
func TestSetSessionTask(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.SetSessionTask(ctx, key, "c1", "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("set on a missing session = %v; want ErrNotFound", err)
	}
	if err := s.UpsertSession(ctx, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	get := func() store.Session {
		sess, err := s.GetSession(ctx, key, 1000*time.Hour, t0)
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}
	for _, tc := range []struct{ name, contextID, taskID, wantContext, wantTask string }{
		{"a task and its context", "c1", "t1", "c1", "t1"},
		{"a later task, no new context", "", "t2", "c1", "t2"},
		{"the task ends", "", "", "c1", ""},
		{"a new context", "c2", "t3", "c2", "t3"},
	} {
		if err := s.TouchSession(ctx, key, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionTask(ctx, key, tc.contextID, tc.taskID); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := get(); got.ContextID != tc.wantContext || got.TaskID != tc.wantTask || got.Binding != "ns/b" || !got.LastActivity.Equal(t0.Add(time.Minute)) {
			t.Errorf("%s: session = %+v; want context %q, task %q, the Binding and the touched activity kept", tc.name, got, tc.wantContext, tc.wantTask)
		}
	}
	if err := s.SetSessionTask(ctx, store.ConversationKey{}, "c", "t"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("set with an empty key = %v; want ErrInvalid", err)
	}
}
