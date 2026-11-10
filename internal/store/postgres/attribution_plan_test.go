package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestUnattributedRowsCountsTheOutboxByTheThreadIndex: the count of outbox rows with no connection can search outbox_thread by
// its first column. The planner reads a table with no statistics whole whatever indexes it has, so the plan is read with
// sequential scans off: what it shows is whether the index can answer the query, not whether the planner picks it.
func TestUnattributedRowsCountsTheOutboxByTheThreadIndex(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	s := openAt(t, dsn)
	ctx := t.Context()
	tx, err := s.a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN `+unattributedOutbox)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan, "\n"); !strings.Contains(got, "outbox_thread") {
		t.Errorf("the count of outbox rows with no connection does not use outbox_thread: %q", got)
	}
}

// TestUnattributedRowsIsNotFenced: a standby counts the rows, like any read, and changes nothing.
func TestUnattributedRowsIsNotFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	leader := openAt(t, dsn)
	if _, err := leader.CreateOutbox(t.Context(), store.OutboxEntry{ID: "old", EventID: "e", Role: store.OutboxNotice, Channel: "C1", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	standby := openStandbyAt(t, dsn)
	got, err := standby.UnattributedRows(t.Context())
	if err != nil || got != (store.AttributedRows{Outbox: 1}) {
		t.Errorf("UnattributedRows of a standby = %+v, %v; want the one outbox row", got, err)
	}
	if _, err := standby.AttributeConnection(t.Context(), "ns/own"); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("AttributeConnection of the standby = %v; want store.ErrDeposed: the count must not have changed that", err)
	}
	if again, err := leader.UnattributedRows(t.Context()); err != nil || again != (store.AttributedRows{Outbox: 1}) {
		t.Errorf("UnattributedRows after the standby's count = %+v, %v; want the one outbox row still: a count changes nothing", again, err)
	}
}
