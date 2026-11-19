package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestMigration0031AddsTheTeamsConversationsTableAndKeepsWhatWasThere: a database at 0030 gains an empty
// teams_conversations, its rows are as they were, and the table takes a record.
func TestMigration0031AddsTheTeamsConversationsTableAndKeepsWhatWasThere(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	db, err := openPool(ctx, path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrationsThrough(t, 30), discardLog); err != nil {
		_ = db.Close()
		t.Fatalf("migrate through 0030: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO served_connections (connection, served_since) VALUES ('ns/teams', %d)`, t0.UnixNano())); err != nil {
		_ = db.Close()
		t.Fatalf("insert a row of the old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	if got, err := s.ServedConnections(ctx); err != nil || len(got) != 1 || got[0] != "ns/teams" {
		t.Fatalf("ServedConnections after 0031 = %v, %v; want the row written before it", got, err)
	}
	if _, err := s.GetTeamsConversation(ctx, "ns/teams", "19:c@thread.tacv2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTeamsConversation after 0031 = %v; want store.ErrNotFound from an empty table", err)
	}
	c := store.TeamsConversation{Connection: "ns/teams", Channel: "19:c@thread.tacv2", ServiceURL: "https://smba.example.test/", UpdatedAt: t0}
	if err := s.RememberTeamsConversation(ctx, c); err != nil {
		t.Fatalf("RememberTeamsConversation after 0031: %v", err)
	}
	if got, err := s.GetTeamsConversation(ctx, c.Connection, c.Channel); err != nil || got.ServiceURL != c.ServiceURL {
		t.Errorf("GetTeamsConversation = %+v, %v; want the record just remembered", got, err)
	}
}

// TestTeamsConversationLookupUsesThePrimaryKey: the read of one channel's record is the primary key's.
func TestTeamsConversationLookupUsesThePrimaryKey(t *testing.T) {
	s := openTemp(t)
	plan := strings.Join(queryPlan(t, s, `SELECT service_url FROM teams_conversations WHERE connection = ? AND channel = ?`, []any{"c", "19:c"}), "\n")
	if !strings.Contains(plan, "sqlite_autoindex_teams_conversations_1") {
		t.Errorf("query plan = %q; want it to use the primary key, sqlite_autoindex_teams_conversations_1", plan)
	}
}
