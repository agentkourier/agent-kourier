package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func auditEntry(subject string, at time.Time) store.AuditEntry {
	return store.AuditEntry{At: at, Kind: store.AuditEvent, Subject: subject, Outcome: "accepted"}
}

// refused reports whether err is the audit trigger's refusal.
func refused(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23001" && pg.Message == "audit is append-only"
}

func countAudit(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.a.pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAuditIsAppendOnly: the audit table takes inserts and nothing else on the application's own paths,
// whatever UPDATE, DELETE or TRUNCATE the application issues; the one way to remove entries is the
// retention purge, which removes only what it is asked to and leaves the table as strict as it was. As
// with SQLite's triggers (migration 0006) this guards against the application's bugs, and not against a
// session that sets agentkourier.audit_retention itself or disables the trigger: that takes the table's owner.
func TestAuditIsAppendOnly(t *testing.T) {
	s, _ := openFresh(t)
	ctx := t.Context()
	old, recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{old, old.Add(time.Hour), recent} {
		if err := s.AppendAudit(ctx, auditEntry(string(rune('a'+i)), at)); err != nil {
			t.Fatal(err)
		}
	}

	for name, sql := range map[string]string{
		"update":   `UPDATE audit SET outcome = 'changed'`,
		"delete":   `DELETE FROM audit`,
		"truncate": `TRUNCATE audit`,
	} {
		if _, err := s.a.pool.Exec(ctx, sql); !refused(err) {
			t.Errorf("%s = %v; want the trigger's refusal", name, err)
		}
	}
	if n := countAudit(t, s); n != 3 {
		t.Fatalf("%d entries after the refused statements; want 3", n)
	}

	// The setting that lets the purge delete is not a way to change an entry, and is gone after the purge.
	for name, sql := range map[string]string{"update": `UPDATE audit SET outcome = 'changed'`, "truncate": `TRUNCATE audit`} {
		tx, err := s.a.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('agentkourier.audit_retention', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql); !refused(err) {
			t.Errorf("%s with the retention setting on = %v; want the trigger's refusal", name, err)
		}
		_ = tx.Rollback(ctx)
	}

	var purged int64
	if err := s.a.pool.QueryRow(ctx, `SELECT agentkourier_audit_purge($1)`, old.Add(24*time.Hour)).Scan(&purged); err != nil {
		t.Fatalf("agentkourier_audit_purge: %v", err)
	}
	if purged != 2 {
		t.Errorf("the purge removed %d entries; want the 2 from before the cutoff", purged)
	}
	l, err := s.ListAudit(ctx, time.Time{}, 0, 10)
	if err != nil || len(l) != 1 || l[0].Subject != "c" {
		t.Errorf("entries after the purge = %+v, %v; want the recent one", l, err)
	}
	if _, err := s.a.pool.Exec(ctx, `DELETE FROM audit`); !refused(err) {
		t.Errorf("delete after the purge = %v; want the trigger's refusal: the purge must not leave the table open", err)
	}

	// The store itself still appends, and its sequence goes on after the purged entries'.
	if err := s.AppendAudit(ctx, auditEntry("d", recent)); err != nil {
		t.Fatal(err)
	}
	if l, _ = s.ListAudit(ctx, time.Time{}, 0, 10); len(l) != 2 || l[1].Sequence <= l[0].Sequence || l[1].Sequence <= 3 {
		t.Errorf("entries = %+v; want the recent one and the new one, with a sequence above all three of the first", l)
	}
}

// PurgeAudit deletes in batches through agentkourier_audit_purge(cutoff, batch) (migration 0023, from 0020's courier_audit_purge), and like the
// unbatched function leaves the table as strict as it was: the setting that lets the trigger through is gone
// when the call returns, and the call gives no way to change an entry.
func TestAuditPurgeInBatchesLeavesTheLogAppendOnly(t *testing.T) {
	s, _ := openFresh(t)
	ctx := t.Context()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		if err := s.AppendAudit(ctx, auditEntry(string(rune('a'+i)), old.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	for want, limit := range []int{2, 2, 1, 0} { // the oldest first, a batch at a time, then nothing left
		n, err := s.PurgeAudit(ctx, old.Add(24*time.Hour), 2)
		if err != nil || n != limit {
			t.Fatalf("batch %d purged %d, %v; want %d", want, n, err, limit)
		}
	}
	if n := countAudit(t, s); n != 0 {
		t.Fatalf("%d entries after the purge; want none", n)
	}
	if err := s.AppendAudit(ctx, auditEntry("f", old)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.a.pool.Exec(ctx, `DELETE FROM audit`); !refused(err) {
		t.Errorf("delete after the batched purge = %v; want the trigger's refusal", err)
	}
	if _, err := s.PurgeAudit(ctx, time.Time{}, 1); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("a purge with no cutoff = %v; want ErrInvalid", err)
	}
}
