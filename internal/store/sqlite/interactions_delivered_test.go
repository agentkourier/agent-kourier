package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestMigration0011TakesExistingAnswersAsDelivered: an answer recorded before there was a mark cannot
// be known undelivered, and none is made a cause for a notice: they are marked delivered when they
// were decided. A pending or timed-out interaction has none.
func TestMigration0011TakesExistingAnswersAsDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
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
	// The whole rows, as they were inserted, with the delivery mark alone new: only an answered one has it.
	key := store.ConversationKey{Connection: "slack", Channel: "C1"}
	for _, want := range []store.Interaction{
		{ID: "answered", Key: withThread(key, "1.1"), TaskID: "t1", Kind: store.InteractionAskUser, State: store.InteractionAnswered, CreatedAt: fromTS(1000), Deadline: fromTS(5000),
			Answer: []byte("{}"), Decider: "U1", DecidedAt: fromTS(2000), DeliveredAt: fromTS(2000)},
		{ID: "pending", Key: withThread(key, "1.2"), TaskID: "t2", Kind: store.InteractionAskUser, State: store.InteractionPending, CreatedAt: fromTS(1000), Deadline: fromTS(5000)},
		{ID: "timedout", Key: withThread(key, "1.3"), TaskID: "t3", Kind: store.InteractionAskUser, State: store.InteractionTimedOut, CreatedAt: fromTS(1000), Deadline: fromTS(1500), DecidedAt: fromTS(1500)},
	} {
		got, err := s.GetInteraction(t.Context(), want.ID)
		if err != nil {
			t.Fatalf("%s after 0011: %v", want.ID, err)
		}
		if !bytes.Equal(got.Request, want.Request) || !bytes.Equal(got.Answer, want.Answer) {
			t.Errorf("%s after 0011: request %q, answer %q; want %q, %q", want.ID, got.Request, got.Answer, want.Request, want.Answer)
		}
		got.Request, got.Answer, want.Request, want.Answer = nil, nil, nil, nil // the slices were compared above
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s after 0011 = %+v; want %+v", want.ID, got, want)
		}
	}
}

func withThread(k store.ConversationKey, ts string) store.ConversationKey {
	k.Thread = ts
	return k
}
