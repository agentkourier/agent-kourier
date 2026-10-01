package sqlite

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestOutboxPayload(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "courier.db")
	s := openAt(t, path)
	if _, err := s.GetOutbox(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOutbox missing = %v; want ErrNotFound", err)
	}
	o := store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", ThreadTS: "1.1", CreatedAt: t0, Payload: `{"text":"hi"}`}
	if c, err := s.CreateOutbox(ctx, o); err != nil || !c {
		t.Fatalf("CreateOutbox = %v, %v", c, err)
	}
	dup := o
	dup.Payload = `{"text":"changed"}`
	if c, err := s.CreateOutbox(ctx, dup); err != nil || c {
		t.Fatalf("duplicate CreateOutbox = %v, %v; want false, nil", c, err)
	}
	// A row written before the payload column existed gets the empty payload.
	if _, err := s.write.ExecContext(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at)
		VALUES ('old', 'e', 'root', 'C1', '', 1)`); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if got, err := s.GetOutbox(ctx, "old"); err != nil || got.Payload != "" {
		t.Fatalf("GetOutbox(old) = %+v, %v; want an empty payload", got, err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = openAt(t, path)
	if got, err := s.GetOutbox(ctx, o.ID); err != nil || got != o {
		t.Fatalf("GetOutbox after reopen = %+v, %v; want %+v", got, err, o)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 2 || l[1] != o { // "old" is older
		t.Fatalf("ListUnposted = %+v, %v; want the payload kept", l, err)
	}

	// Recording the ref clears the payload: the text lives in Slack, not in the database too.
	for range 2 { // the second record is the same-ref no-op, and still leaves it empty
		if err := s.RecordMessageRef(ctx, o.ID, "1700.01"); err != nil {
			t.Fatalf("RecordMessageRef: %v", err)
		}
		want := o
		want.Payload, want.MessageRef = "", "1700.01"
		if got, err := s.GetOutbox(ctx, o.ID); err != nil || got != want {
			t.Fatalf("GetOutbox after record = %+v, %v; want %+v", got, err, want)
		}
	}
	if err := s.RecordMessageRef(ctx, o.ID, "1700.99"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a different ref = %v; want ErrConflict", err)
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
	path := filepath.Join(t.TempDir(), "courier.db")
	databaseAt(t, path, 6)
	s := openAt(t, path) // applies 0007
	ctx := context.Background()
	if got, err := s.GetOutbox(ctx, "unposted"); err != nil || got.Payload != "" || got.MessageRef != "" {
		t.Fatalf("unposted row after 0007 = %+v, %v", got, err)
	}
	if got, err := s.GetOutbox(ctx, "posted"); err != nil || got.MessageRef != "5.5" || got.ThreadTS != "1.1" {
		t.Fatalf("posted row after 0007 = %+v, %v", got, err)
	}
}

// TestMigration0010KeepsExistingRowsAndTakesTheReactionRole opens a database migrated to 0009 with
// outbox rows in it: 0010 replaces the role triggers over existing rows, which must read back, and
// the reaction role, which 0009's triggers refused, is accepted from then on.
func TestMigration0010KeepsExistingRowsAndTakesTheReactionRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	databaseAt(t, path, 9)
	s := openAt(t, path) // applies 0010
	ctx := context.Background()
	if got, err := s.GetOutbox(ctx, "unposted"); err != nil || got.Role != store.OutboxRoot || got.MessageRef != "" {
		t.Fatalf("unposted row after 0010 = %+v, %v", got, err)
	}
	if got, err := s.GetOutbox(ctx, "posted"); err != nil || got.Role != store.OutboxReply || got.MessageRef != "5.5" || got.ThreadTS != "1.1" {
		t.Fatalf("posted row after 0010 = %+v, %v", got, err)
	}
	e := store.OutboxEntry{ID: "r/reaction", EventID: "r", Role: store.OutboxReaction, Channel: "C1", CreatedAt: t0, Payload: `{"ts":"1.5","name":"hourglass"}`}
	if created, err := s.CreateOutbox(ctx, e); err != nil || !created {
		t.Fatalf("CreateOutbox with the reaction role = %v, %v; want it created", created, err)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 2 {
		t.Fatalf("ListUnposted = %+v, %v; want the old unposted row and the reaction", l, err)
	}
}
