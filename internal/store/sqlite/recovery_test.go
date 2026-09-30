package sqlite

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestListActiveSessions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if got, err := s.ListActiveSessions(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store = %v, %v; want none", got, err)
	}
	mk := func(thread, taskID string, age time.Duration) store.Session {
		return store.Session{
			Key:       store.ConversationKey{Connection: "slack", Channel: "C1", ThreadTS: thread},
			ContextID: "ctx-" + thread, TaskID: taskID, Binding: "ns/b", LastActivity: t0.Add(-age),
		}
	}
	// "old" is far past any TTL: an expired session with an active task is still listed,
	// and the caller decides what to do with it.
	for _, sess := range []store.Session{mk("3.3", "task-c", time.Hour), mk("1.1", "task-a", 2*time.Hour), mk("idle", "", 0), mk("old", "task-o", 4000*time.Hour)} {
		if err := s.UpsertSession(ctx, sess); err != nil {
			t.Fatalf("upsert %s: %v", sess.Key.ThreadTS, err)
		}
	}
	before, _ := s.GetSession(ctx, mk("3.3", "", 0).Key, 1000*time.Hour, t0)
	got, err := s.ListActiveSessions(ctx)
	if err != nil {
		t.Fatalf("ListActiveSessions: %v", err)
	}
	var threads, tasks []string
	for _, sess := range got {
		threads, tasks = append(threads, sess.Key.ThreadTS), append(tasks, sess.TaskID)
		if sess.Binding != "ns/b" || sess.ContextID != "ctx-"+sess.Key.ThreadTS || sess.LastActivity.IsZero() {
			t.Errorf("session %+v lost a field", sess)
		}
	}
	if want := []string{"old", "1.1", "3.3"}; !slices.Equal(threads, want) { // oldest activity first
		t.Fatalf("threads = %v; want %v", threads, want)
	}
	if want := []string{"task-o", "task-a", "task-c"}; !slices.Equal(tasks, want) {
		t.Fatalf("tasks = %v; want %v", tasks, want)
	}
	if after, _ := s.GetSession(ctx, mk("3.3", "", 0).Key, 1000*time.Hour, t0); !after.LastActivity.Equal(before.LastActivity) {
		t.Fatalf("listing changed LastActivity: %v -> %v", before.LastActivity, after.LastActivity)
	}
}

func TestListQueuedSessions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if got, err := s.ListQueuedSessions(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store = %v, %v; want none", got, err)
	}
	k := func(thread string) store.ConversationKey {
		return store.ConversationKey{Connection: "slack", Channel: "C1", ThreadTS: thread}
	}
	// b's head comes first, though a has more rows and a's later ones interleave.
	for _, r := range []struct{ id, thread string }{{"b1", "b"}, {"a1", "a"}, {"b2", "b"}, {"a2", "a"}, {"d1", "delivered"}} {
		if _, err := s.EnqueueReply(ctx, newReply(r.id, k(r.thread))); err != nil {
			t.Fatalf("enqueue %s: %v", r.id, err)
		}
	}
	if err := s.MarkReplyDelivered(ctx, "d1", t0); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, err := s.ListQueuedSessions(ctx)
	if err != nil {
		t.Fatalf("ListQueuedSessions: %v", err)
	}
	if want := []store.ConversationKey{k("b"), k("a")}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v; want %v (each once, oldest head first, no tombstone-only session)", got, want)
	}
	// Delivering b's whole backlog drops it; a's head is now the oldest.
	for _, id := range []string{"b1", "b2"} {
		if err := s.MarkReplyDelivered(ctx, id, t0); err != nil {
			t.Fatalf("mark %s: %v", id, err)
		}
	}
	if got, _ := s.ListQueuedSessions(ctx); !slices.Equal(got, []store.ConversationKey{k("a")}) {
		t.Fatalf("keys after draining b = %v; want [a]", got)
	}
}

func TestRecoveryListsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "courier.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.UpsertSession(ctx, store.Session{Key: key, ContextID: "c", TaskID: "t", Binding: "ns/b", LastActivity: t0}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.EnqueueReply(ctx, newReply("r1", key)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = openAt(t, path)
	if got, err := s.ListActiveSessions(ctx); err != nil || len(got) != 1 || got[0].TaskID != "t" {
		t.Fatalf("active after reopen = %+v, %v", got, err)
	}
	if got, err := s.ListQueuedSessions(ctx); err != nil || !slices.Equal(got, []store.ConversationKey{key}) {
		t.Fatalf("queued after reopen = %v, %v", got, err)
	}
}
