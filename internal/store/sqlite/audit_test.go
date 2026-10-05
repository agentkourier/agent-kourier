package sqlite

import (
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
