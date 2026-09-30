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
	for range 2 { // a repeat of the same call is not an error, though no row changes
		if err := s.SetSessionTask(ctx, key, "c2", "t3"); err != nil {
			t.Errorf("repeated set = %v; want nil", err)
		}
	}
	if err := s.SetSessionTask(ctx, store.ConversationKey{}, "c", "t"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("set with an empty key = %v; want ErrInvalid", err)
	}
}

// ResetSession forgets the agent's session, and only that: the Binding and the activity stay,
// so the thread is still owned, and the next turn starts a fresh context (spec 8.2).
func TestResetSession(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.ResetSession(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reset of a missing session = %v; want ErrNotFound", err)
	}
	if err := s.UpsertSession(ctx, store.Session{Key: key, ContextID: "c1", TaskID: "t1", Binding: "ns/b", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // and again: a repeat changes nothing
		if err := s.ResetSession(ctx, key); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSession(ctx, key, 1000*time.Hour, t0)
		if want := (store.Session{Key: key, Binding: "ns/b", LastActivity: t0}); err != nil || got != want {
			t.Fatalf("session = %+v, %v; want %+v", got, err, want)
		}
	}
	if err := s.ResetSession(ctx, store.ConversationKey{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("reset with an empty key = %v; want ErrInvalid", err)
	}
}
