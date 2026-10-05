package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// The interface has no update or delete for the audit log, and the suite holds every store to that. This is
// how SQLite makes it true of the file too: a trigger refuses both statements, even from raw SQL.
func TestAuditIsAppendOnlyInTheDatabase(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	e := store.AuditEntry{At: t0, Kind: store.AuditInteractionDecision, Subject: "a", Outcome: "approved"}
	if err := s.AppendAudit(ctx, e); err != nil {
		t.Fatalf("append: %v", err)
	}
	for _, q := range []string{`UPDATE audit SET outcome = 'edited'`, `DELETE FROM audit`} {
		if _, err := s.write.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q = %v; want the append-only error", q, err)
		}
	}
	if l, err := s.ListAudit(ctx, time.Time{}, 0, 10); err != nil || len(l) != 1 || l[0].Outcome != "approved" {
		t.Fatalf("audit after tampering attempts = %+v, %v", l, err)
	}
}

// The purge is the one way an entry leaves the log. The delete trigger lets a DELETE through only while
// audit_purge holds a cutoff later than the entry, and the purge leaves no row behind, so every other path is as
// refused as before. The marker is not a way to change an entry, and a marker left by hand frees only what is
// before its cutoff.
func TestAuditPurgeLeavesTheLogAppendOnly(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	for i, at := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		e := store.AuditEntry{At: t0.Add(at), Kind: store.AuditInteractionDecision, Subject: string(rune('a' + i)), Outcome: "approved"}
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if n, err := s.PurgeAudit(ctx, t0.Add(time.Hour), 10); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want the one entry before the cutoff", n, err)
	}
	var markers int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM audit_purge`).Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("markers after the purge = %d, %v; want none: the purge must not leave the table open", markers, err)
	}
	for _, q := range []string{`UPDATE audit SET outcome = 'edited'`, `DELETE FROM audit`} {
		if _, err := s.write.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q after the purge = %v; want the append-only error", q, err)
		}
	}

	// A failed purge, which rolls back, leaves no marker either.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PurgeAudit(cancelled, t0.Add(24*time.Hour), 10); err == nil {
		t.Error("a purge with an ended context succeeded")
	}
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM audit_purge`).Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("markers after a failed purge = %d, %v; want none", markers, err)
	}

	// A marker, even one that is not meant to be there, frees only the entries before its cutoff, and no update.
	if _, err := s.write.ExecContext(ctx, `INSERT INTO audit_purge (cutoff) VALUES (?)`, ts(t0.Add(90*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.ExecContext(ctx, `UPDATE audit SET outcome = 'edited'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("update with a marker present = %v; want the append-only error", err)
	}
	if _, err := s.write.ExecContext(ctx, `DELETE FROM audit WHERE subject = 'c'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("delete of an entry after the marker's cutoff = %v; want the append-only error", err)
	}
	if _, err := s.write.ExecContext(ctx, `DELETE FROM audit WHERE subject = 'b'`); err != nil {
		t.Errorf("delete of an entry before the marker's cutoff: %v", err)
	}
	if l, err := s.ListAudit(ctx, time.Time{}, 0, 10); err != nil || len(l) != 1 || l[0].Subject != "c" {
		t.Errorf("audit at the end = %+v, %v; want only c", l, err)
	}
}
