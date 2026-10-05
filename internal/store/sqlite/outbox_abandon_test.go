package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigration0008KeepsExistingRows opens a database migrated to 0007: the rows are kept, as not
// abandoned, and the unposted one is still work.
func TestMigration0008KeepsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	databaseAt(t, path, 7)
	s := openAt(t, path) // applies 0008
	ctx := context.Background()
	got, err := s.GetOutbox(ctx, "unposted")
	if err != nil || !got.AbandonedAt.IsZero() {
		t.Fatalf("row after 0008 = %+v, %v; want it kept, not abandoned", got, err)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 1 || l[0].ID != "unposted" {
		t.Fatalf("ListUnposted = %+v, %v; want the unposted row", l, err)
	}
	if got, err := s.GetOutbox(ctx, "posted"); err != nil || got.MessageRef != "5.5" {
		t.Fatalf("posted row after 0008 = %+v, %v", got, err)
	}
}
