package sqlite

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/agentkourier/agent-kourier/internal/session"
)

// The migration that renamed the stored system user, and the user's value before it. The value after it is
// session.SystemUser itself, so that a constant changed without a migration, or a migration written for another
// value, fails here.
const (
	systemUserMigration = 24
	oldSystemUser       = "courier:system"
)

// TestMigration0024RenamesTheStoredSystemUser opens a database migrated to 0023 with a system turn and a person's
// turn in it, in the reply queue and in the session's current turn, and an auto-denied approval and a person's
// decision: 0024 moves the system user, which must be session.SystemUser, and the auto decider to their new names,
// and leaves the person's alone. A turn queued or running before the upgrade must still read as the system's.
func TestMigration0024RenamesTheStoredSystemUser(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "agent-kourier.db")

	old := fstest.MapFS{}
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil || len(files) < systemUserMigration {
		t.Fatalf("migrations = %v, %v", files, err)
	}
	for _, f := range files[:systemUserMigration-1] {
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
	for id, user := range map[string]string{"system-turn": oldSystemUser, "person-turn": "U1"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO reply_queue (id, connection, channel, thread_ts, user_id, message_ts, text, enqueued_at)
			VALUES (?, 'slack', 'C1', ?, ?, '1.1', 'hello', 1)`, id, id, user); err != nil {
			t.Fatalf("insert a reply: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sessions (connection, channel, thread_ts, context_id, task_id, binding, last_activity, turn_id, turn_user)
			VALUES ('slack', 'C1', ?, 'ctx', 'task', 'sre/sre-alerts', 1, 'turn-1', ?)`, id, user); err != nil {
			t.Fatalf("insert a session: %v", err)
		}
	}
	for id, decider := range map[string]string{"auto-denied": "courier", "person-decided": "U1"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO interactions (id, connection, channel, thread_ts, task_id, kind, state, request, created_at, deadline, decider, decided_at)
			VALUES (?, 'slack', 'C1', 'ix', 'task', 'tool_approval', 'answered', '{}', 1, 2, ?, 2)`, id, decider); err != nil {
			t.Fatalf("insert an interaction: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path) // applies 0024
	for _, tc := range []struct{ query, id, want string }{
		{`SELECT user_id FROM reply_queue WHERE id = ?`, "system-turn", session.SystemUser},
		{`SELECT user_id FROM reply_queue WHERE id = ?`, "person-turn", "U1"},
		{`SELECT turn_user FROM sessions WHERE thread_ts = ?`, "system-turn", session.SystemUser},
		{`SELECT turn_user FROM sessions WHERE thread_ts = ?`, "person-turn", "U1"},
		{`SELECT decider FROM interactions WHERE id = ?`, "auto-denied", "agent-kourier"},
		{`SELECT decider FROM interactions WHERE id = ?`, "person-decided", "U1"},
	} {
		var got string
		if err := s.a.write.QueryRowContext(ctx, tc.query, tc.id).Scan(&got); err != nil || got != tc.want {
			t.Errorf("%s for %s = %q, %v; want %q", tc.query, tc.id, got, err, tc.want)
		}
	}
}
