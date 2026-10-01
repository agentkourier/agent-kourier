package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// MarkInteractionDelivered records, once, that an answer reached the session: the first mark stands, a
// repeat is no error, and only an answered interaction has an answer to deliver.
func TestMarkInteractionDelivered(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.CreateInteraction(ctx, newInteraction("i1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInteractionDelivered(ctx, "i1", t0.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mark of a pending interaction = %v; want ErrConflict: there is no answer yet", err)
	}
	if err := s.SubmitInteraction(ctx, "i1", []byte(`{"a":1}`), "U1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetInteraction(ctx, "i1"); !got.DeliveredAt.IsZero() {
		t.Fatalf("an answer just recorded is delivered at %v; want it not known to be", got.DeliveredAt)
	}
	for _, at := range []time.Time{t0.Add(2 * time.Minute), t0.Add(3 * time.Minute)} { // the second is a repeat
		if err := s.MarkInteractionDelivered(ctx, "i1", at); err != nil {
			t.Fatalf("mark at %v = %v", at, err)
		}
	}
	got, _ := s.GetInteraction(ctx, "i1")
	if !got.DeliveredAt.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("delivered at %v; want the first mark, %v", got.DeliveredAt, t0.Add(2*time.Minute))
	}
	if listed, err := s.ListDecidedInteractions(ctx, t0); err != nil || len(listed) != 1 || !listed[0].DeliveredAt.Equal(got.DeliveredAt) {
		t.Errorf("listed %+v, %v; want the delivery time with the row", listed, err)
	}
	if _, err := s.CreateInteraction(ctx, newInteraction("i2", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveInteraction(ctx, "i2", store.InteractionCancelled, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInteractionDelivered(ctx, "i2", t0.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("mark of a cancelled interaction = %v; want ErrConflict", err)
	}
	if err := s.MarkInteractionDelivered(ctx, "nope", t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("mark of a missing interaction = %v; want ErrNotFound", err)
	}
	for name, call := range map[string]func() error{
		"no ID":   func() error { return s.MarkInteractionDelivered(ctx, "", t0) },
		"no time": func() error { return s.MarkInteractionDelivered(ctx, "i1", time.Time{}) },
	} {
		if err := call(); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
}

// TestMigration0011TakesExistingAnswersAsDelivered: an answer recorded before there was a mark cannot
// be known undelivered, and none is made a cause for a notice: they are marked delivered when they
// were decided. A pending or timed-out interaction has none.
func TestMigration0011TakesExistingAnswersAsDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	databaseAt(t, path, 10)
	db, err := openPool(context.Background(), path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, state, request, created_at, deadline, answer, decider, decided_at)
			VALUES ('answered', 'slack', 'C1', '1.1', 't1', 'ask_user', 'answered', x'', 1000, 5000, x'7b7d', 'U1', 2000)`,
		`INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, state, request, created_at, deadline)
			VALUES ('pending', 'slack', 'C1', '1.2', 't2', 'ask_user', 'pending', x'', 1000, 5000)`,
		`INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, state, request, created_at, deadline, decided_at)
			VALUES ('timedout', 'slack', 'C1', '1.3', 't3', 'ask_user', 'timed_out', x'', 1000, 1500, 1500)`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path) // applies 0011
	for id, want := range map[string]time.Time{"answered": fromTS(2000), "pending": {}, "timedout": {}} {
		got, err := s.GetInteraction(t.Context(), id)
		if err != nil || !got.DeliveredAt.Equal(want) {
			t.Errorf("%s after 0011: delivered at %v, %v; want %v", id, got.DeliveredAt, err, want)
		}
	}
}
