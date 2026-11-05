package sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestAFullDatabaseFailsTheWritesAndKeepsTheReads holds what docs/reference/limits.md says of a full SQLite volume. SQLite's
// max_page_count stands in for the volume: it bounds the database file, where a volume that is full bounds the file system under
// the database and its write-ahead log too, which this does not try. The audit log is the table that grows without a retention.
func TestAFullDatabaseFailsTheWritesAndKeepsTheReads(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var pages int
	if err := s.a.write.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.a.write.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", pages+40)); err != nil {
		t.Fatal(err)
	}
	entry := func(subject string) store.AuditEntry {
		return store.AuditEntry{At: time.Now(), Kind: store.AuditEvent, Binding: "ns/b", Subject: subject, Outcome: "accepted"}
	}

	var full error
	appended := 0
	for ; appended < 100000 && full == nil; appended++ {
		full = s.AppendAudit(ctx, entry(fmt.Sprintf("subject-%d", appended)))
	}
	if full == nil || !strings.Contains(full.Error(), "database or disk is full") {
		t.Fatalf("after %d appends the error is %v; want the SQLite full-disk error", appended, full)
	}
	if err := s.AppendAudit(ctx, entry("again")); err == nil {
		t.Error("an append after the first failure succeeded: the database was not full")
	}
	if l, err := s.ListAudit(ctx, time.Time{}, 0, 5); err != nil || len(l) != 5 {
		t.Errorf("a read of a full database = %d entries, %v; want 5 and no error", len(l), err)
	}
	if _, err := s.a.write.ExecContext(ctx, "DELETE FROM audit WHERE sequence <= 200"); err == nil {
		t.Error("a plain delete of audit rows succeeded: the log is append-only")
	}

	// The retention purge is the one way entries leave, and it ran on the full database: it frees room, and appends work again.
	purged, err := s.PurgeAudit(ctx, time.Now().Add(time.Hour), 200)
	if err != nil || purged == 0 {
		t.Fatalf("PurgeAudit on a full database = %d, %v; want entries purged", purged, err)
	}
	if err := s.AppendAudit(ctx, entry("after the purge")); err != nil {
		t.Errorf("an append after the purge: %v; want it to work", err)
	}
}
