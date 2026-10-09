package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// A row written before the payload column existed, or by a writer that does not know it, reads with an empty
// payload: the column's default is what the interface's "no payload" is in the file.
func TestOutboxRowWithoutAPayloadColumnReadsEmpty(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	if _, err := s.a.write.ExecContext(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at)
		VALUES ('old', 'e', 'root', 'C1', '', 1)`); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if got, err := s.GetOutbox(ctx, "old"); err != nil || got.Payload != "" {
		t.Fatalf("GetOutbox(old) = %+v, %v; want an empty payload", got, err)
	}
}

// databaseAt creates the database at path with the migrations up to number last only, and two
// outbox rows in it, one posted and one not.
func databaseAt(t *testing.T, path string, last int) {
	t.Helper()
	ctx := context.Background()
	old := fstest.MapFS{}
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil || len(files) < last {
		t.Fatalf("migrations = %v, %v", files, err)
	}
	for _, f := range files[:last] {
		data, err := migrationFiles.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		old[f] = &fstest.MapFile{Data: data}
	}
	db, err := openPool(ctx, path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, message_ref) VALUES
		('unposted', 'e', 'root', 'C1', '', 1, ''), ('posted', 'e', 'reply', 'C1', '1.1', 2, '5.5')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMigration0007KeepsExistingRows opens a database migrated to 0006 with outbox rows in it:
// 0007 runs over existing rows and must keep them.
func TestMigration0007KeepsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	databaseAt(t, path, 6)
	s := openAt(t, path) // applies 0007
	ctx := context.Background()
	if got, err := s.GetOutbox(ctx, "unposted"); err != nil || got.Payload != "" || got.MessageRef != "" {
		t.Fatalf("unposted row after 0007 = %+v, %v", got, err)
	}
	if got, err := s.GetOutbox(ctx, "posted"); err != nil || got.MessageRef != "5.5" || got.Thread != "1.1" {
		t.Fatalf("posted row after 0007 = %+v, %v", got, err)
	}
}

// TestMigration0010KeepsExistingRowsAndTakesTheReactionRole opens a database migrated to 0009 with
// outbox rows in it: 0010 replaces the role triggers over existing rows, which must read back, and
// the reaction role, which 0009's triggers refused, is accepted from then on.
func TestMigration0010KeepsExistingRowsAndTakesTheReactionRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	databaseAt(t, path, 9)
	s := openAt(t, path) // applies 0010
	ctx := context.Background()
	// The whole rows, as databaseAt wrote them: a migration that rewrote any column would show here.
	for _, want := range []store.OutboxEntry{
		{ID: "unposted", EventID: "e", Role: store.OutboxRoot, Channel: "C1", CreatedAt: fromTS(1)},
		{ID: "posted", EventID: "e", Role: store.OutboxReply, Channel: "C1", Thread: "1.1", CreatedAt: fromTS(2), MessageRef: "5.5"},
	} {
		if got, err := s.GetOutbox(ctx, want.ID); err != nil || got != want {
			t.Fatalf("%s row after 0010 = %+v, %v; want %+v", want.ID, got, err, want)
		}
	}
	e := store.OutboxEntry{ID: "r/reaction", EventID: "r", Role: store.OutboxReaction, Channel: "C1", CreatedAt: t0, Payload: `{"ts":"1.5","name":"hourglass"}`}
	if created, err := s.CreateOutbox(ctx, e); err != nil || !created {
		t.Fatalf("CreateOutbox with the reaction role = %v, %v; want it created", created, err)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 2 {
		t.Fatalf("ListUnposted = %+v, %v; want the old unposted row and the reaction", l, err)
	}
}
