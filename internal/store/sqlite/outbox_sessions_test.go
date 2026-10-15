package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var key = store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}

// The role set is checked in Go (OutboxRole.Valid) and by a trigger, and both know the same roles, the reaction
// among them. A role the trigger does not list is refused even to raw SQL, and one it lists is allowed.
func TestOutboxRoleTriggers(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	a := store.OutboxEntry{ID: "ev1/root", EventID: "ev1", Role: store.OutboxRoot, Channel: "C1", CreatedAt: t0}
	if _, err := s.CreateOutbox(ctx, a); err != nil {
		t.Fatalf("CreateOutbox: %v", err)
	}
	if _, err := s.a.write.ExecContext(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at)
		VALUES ('raw', 'e', 'bogus', 'C', '', 1)`); err == nil || !strings.Contains(err.Error(), "invalid outbox role") {
		t.Fatalf("insert with an invalid outbox role = %v; want the trigger's error", err)
	}
	if _, err := s.a.write.ExecContext(ctx, `UPDATE outbox SET role = 'bogus' WHERE id = ?`, a.ID); err == nil || !strings.Contains(err.Error(), "invalid outbox role") {
		t.Fatalf("update to an invalid outbox role = %v; want the trigger's error", err)
	}
	for _, role := range []store.OutboxRole{store.OutboxRoot, store.OutboxReply, store.OutboxDigest, store.OutboxNotice, store.OutboxReaction} {
		if _, err := s.a.write.ExecContext(ctx, `UPDATE outbox SET role = ? WHERE id = ?`, role, a.ID); err != nil {
			t.Errorf("update to role %q = %v; want it allowed", role, err)
		}
	}
}

// Every migration is recorded once, however many times the file is opened.
func TestMigrationsAreRecordedOnceAcrossReopens(t *testing.T) {
	path := t.TempDir() + "/agent-kourier.db"
	openAt(t, path)
	s := openAt(t, path)
	var n int
	if err := s.a.read.QueryRowContext(t.Context(), "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != 26 {
		t.Fatalf("schema_migrations rows after reopen = %d, %v; want 26", n, err)
	}
}

func TestInTxRollsBackOnPanic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openTemp(t)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = s.a.inTx(ctx, func(*sql.Tx) error { panic("boom") })
	}()
	if recovered == nil {
		t.Fatal("inTx swallowed the panic")
	}
	// A leaked Tx would hold the only write connection and time this out.
	if _, err := s.AppendEvent(ctx, store.Event{ID: "after-panic", Kind: "alert", ReceivedAt: t0}); err != nil {
		t.Fatalf("write after a panicking transaction: %v", err)
	}
}
