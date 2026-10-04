package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func newReply(id string, k store.ConversationKey) store.Reply {
	return store.Reply{ID: id, Key: k, UserID: "U1", MessageRef: "1700.01", Text: "hello", EnqueuedAt: t0}
}

// HasReply says whether a turn was ever durably queued, which a delivered one still was: the tombstone answers.
func TestHasReplySeesQueuedAndDeliveredReplies(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	has := func(id string) bool {
		t.Helper()
		ok, err := s.HasReply(ctx, id)
		if err != nil {
			t.Fatalf("HasReply(%q): %v", id, err)
		}
		return ok
	}
	if has("r1") {
		t.Fatal("HasReply of an ID never enqueued is true")
	}
	if _, err := s.EnqueueReply(ctx, newReply("r1", key)); err != nil {
		t.Fatal(err)
	}
	if !has("r1") || has("r2") {
		t.Fatal("HasReply after enqueueing r1: want r1 yes, r2 no")
	}
	if err := s.MarkReplyDelivered(ctx, "r1", t0); err != nil {
		t.Fatal(err)
	}
	if !has("r1") {
		t.Error("HasReply of a delivered reply is false; want the tombstone to answer yes")
	}
	if _, err := s.HasReply(ctx, ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("HasReply with no ID = %v; want ErrInvalid", err)
	}
}

func TestReplyQueueIsFIFOPerSession(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	other := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "2.2"}
	if _, err := s.NextReply(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("next on empty queue = %v; want ErrNotFound", err)
	}
	// Enqueue times run backwards on purpose: order must follow the database sequence.
	for i, r := range []store.Reply{newReply("r1", key), newReply("x1", other), newReply("r2", key), newReply("r3", key)} {
		r.EnqueuedAt = t0.Add(-time.Duration(i) * time.Hour)
		if ok, err := s.EnqueueReply(ctx, r); err != nil || !ok {
			t.Fatalf("enqueue %s = %v, %v", r.ID, ok, err)
		}
	}
	if ok, err := s.EnqueueReply(ctx, newReply("r2", key)); err != nil || ok {
		t.Fatalf("duplicate enqueue = %v, %v; want false, nil", ok, err)
	}
	for _, want := range []string{"r1", "r2", "r3"} {
		got, err := s.NextReply(ctx, key)
		if err != nil || got.ID != want || got.UserID != "U1" || got.Text != "hello" || got.MessageRef != "1700.01" || got.Key != key || got.Sequence == 0 {
			t.Fatalf("next = %+v, %v; want %s", got, err, want)
		}
		if again, _ := s.NextReply(ctx, key); again.ID != want {
			t.Fatalf("NextReply does not leave the head in place: %s", again.ID)
		}
		if err := s.MarkReplyDelivered(ctx, want, t0); err != nil {
			t.Fatalf("mark %s delivered: %v", want, err)
		}
	}
	if _, err := s.NextReply(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("next after draining = %v; want ErrNotFound", err)
	}
	if got, err := s.NextReply(ctx, other); err != nil || got.ID != "x1" {
		t.Fatalf("other session's queue = %+v, %v; want x1 untouched", got, err)
	}
	// Delivered replies stay as tombstones: marking again is a no-op, the text is gone,
	// and the same event ID is not queued a second time.
	if err := s.MarkReplyDelivered(ctx, "r1", t0.Add(time.Hour)); err != nil {
		t.Fatalf("mark twice: %v", err)
	}
	if ok, err := s.EnqueueReply(ctx, newReply("r1", key)); err != nil || ok {
		t.Fatalf("re-enqueue of a delivered ID = %v, %v; want false, nil", ok, err)
	}
	if _, err := s.NextReply(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delivered reply came back: %v", err)
	}
	var text string
	var delivered int64
	if err := s.read.QueryRowContext(ctx, `SELECT text, delivered_at FROM reply_queue WHERE id = 'r1'`).Scan(&text, &delivered); err != nil || text != "" || delivered != ts(t0) {
		t.Fatalf("tombstone = %q, %d, %v; want no text and the first delivery time", text, delivered, err)
	}
	if err := s.MarkReplyDelivered(ctx, "nope", t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("mark unknown = %v; want ErrNotFound", err)
	}
	for _, bad := range []func() error{
		func() error { return s.MarkReplyDelivered(ctx, "", t0) },
		func() error { return s.MarkReplyDelivered(ctx, "r1", time.Time{}) },
	} {
		if err := bad(); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("invalid mark = %v; want ErrInvalid", err)
		}
	}
}

func TestReplyQueueValidation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for name, mutate := range map[string]func(*store.Reply){
		"empty ID":       func(r *store.Reply) { r.ID = "" },
		"empty key part": func(r *store.Reply) { r.Key.Thread = "" },
		"empty user":     func(r *store.Reply) { r.UserID = "" },
		"empty ts":       func(r *store.Reply) { r.MessageRef = "" },
		"zero time":      func(r *store.Reply) { r.EnqueuedAt = time.Time{} },
	} {
		bad := newReply("bad", key)
		mutate(&bad)
		if _, err := s.EnqueueReply(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: enqueue = %v; want ErrInvalid", name, err)
		}
	}
	if _, err := s.NextReply(ctx, store.ConversationKey{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("next with empty key = %v; want ErrInvalid", err)
	}
}

func TestReplyQueueConcurrentEnqueueKeepsOneSequence(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = s.EnqueueReply(ctx, newReply(fmt.Sprintf("r%02d", i), key)) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	var last int64
	for range n {
		got, err := s.NextReply(ctx, key)
		if err != nil || got.Sequence <= last {
			t.Fatalf("next = %+v, %v; want a sequence above %d", got, err, last)
		}
		last = got.Sequence
		if err := s.MarkReplyDelivered(ctx, got.ID, t0); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	}
}
