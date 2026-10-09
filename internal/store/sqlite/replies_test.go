package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// A delivered reply stays as a tombstone: the interface promises that its ID is not queued again, and in the
// file its text is gone and its first delivery time is kept.
func TestDeliveredReplyKeepsATombstoneWithoutItsText(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	r := store.Reply{ID: "r1", Key: key, UserID: "U1", MessageRef: "1700.01", Text: "hello", EnqueuedAt: t0}
	if _, err := s.EnqueueReply(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{t0, t0.Add(time.Hour)} { // the second mark is a no-op
		if err := s.MarkReplyDelivered(ctx, "r1", at); err != nil {
			t.Fatalf("mark %v: %v", at, err)
		}
	}
	var text string
	var delivered int64
	if err := s.a.read.QueryRowContext(ctx, `SELECT text, delivered_at FROM reply_queue WHERE id = 'r1'`).Scan(&text, &delivered); err != nil || text != "" || delivered != ts(t0) {
		t.Fatalf("tombstone = %q, %d, %v; want no text and the first delivery time", text, delivered, err)
	}
}

// A database from current main must gain the reply claim without losing queued
// turns or the trace context persisted by migration 12.
func TestReplyClaimMigrationUpgradesCurrentMain(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	databaseAt(t, path, 14)
	db, err := openPool(ctx, path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, text, enqueued_at, trace_parent, trace_state) VALUES ('upgrade', 'slack', 'C1', '1.1', 'U1', '1.2', 'answer', ?, 'parent', 'state')`, ts(t0))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	key := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	r, err := s.NextReply(ctx, key)
	if err != nil || r.Text != "answer" || r.InteractionID != "" || r.TraceParent != "parent" || r.TraceState != "state" {
		t.Fatalf("upgraded reply = %+v, %v", r, err)
	}
	if err := s.ClaimReplyInteraction(ctx, r.ID, "question"); err != nil {
		t.Fatal(err)
	}
	r, err = s.NextReplyAfter(ctx, key, 0)
	if err != nil || r.InteractionID != "question" || r.TraceParent != "parent" || r.TraceState != "state" {
		t.Fatalf("claimed upgraded reply = %+v, %v", r, err)
	}
}

// PreviousReply runs on every mentioned turn of a mention-only Binding. Migration 22's index of a thread, which ends in
// the sequence as SQLite's rowid, answers it by a seek: no table scan and no sort of the thread's rows. A plan that
// misses it fails here, so an index dropped or a query reworded is seen (TM-5 needed no index of its own for this).
func TestPreviousReplyUsesTheThreadIndex(t *testing.T) {
	plan := strings.Join(queryPlan(t, openTemp(t), previousReplyQuery, []any{"slack", "C1", "1.1", int64(5)}), "\n")
	if !strings.Contains(plan, "USING INDEX reply_queue_thread (connection=? AND channel=? AND thread_ts=? AND rowid<?)") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("plan = %q; want a seek of reply_queue_thread to the rows below the sequence, with no sort", plan)
	}
}
