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

// TestMigration0032AddsTheTeamsPollPositionsTableAndKeepsWhatWasThere: a database at 0031 gains an empty teams_poll_positions, its
// rows are as they were, and the table takes a position.
func TestMigration0032AddsTheTeamsPollPositionsTableAndKeepsWhatWasThere(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	db, err := openPool(ctx, path, 1, "journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrationsThrough(t, 31), discardLog); err != nil {
		_ = db.Close()
		t.Fatalf("migrate through 0031: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO teams_conversations (connection, channel, service_url, team_id, aad_group_id, tenant_id, updated_at)
		VALUES ('ns/teams', '19:c@thread.tacv2', 'https://smba.example.test/', '', '', '', %d)`, t0.UnixNano())); err != nil {
		_ = db.Close()
		t.Fatalf("insert a row of the old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	if got, err := s.GetTeamsConversation(ctx, "ns/teams", "19:c@thread.tacv2"); err != nil || got.ServiceURL != "https://smba.example.test/" {
		t.Fatalf("GetTeamsConversation after 0032 = %+v, %v; want the row written before it", got, err)
	}
	if _, err := s.GetTeamsPollPosition(ctx, "ns/teams", "19:c@thread.tacv2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTeamsPollPosition after 0032 = %v; want store.ErrNotFound from an empty table", err)
	}
	p := store.TeamsPollPosition{Connection: "ns/teams", Channel: "19:c@thread.tacv2", DeltaLink: "https://graph.example.test/delta?$deltatoken=1", PolledAt: t0}
	if err := s.SaveTeamsPollPosition(ctx, p); err != nil {
		t.Fatalf("SaveTeamsPollPosition after 0032: %v", err)
	}
	if got, err := s.GetTeamsPollPosition(ctx, p.Connection, p.Channel); err != nil || got.DeltaLink != p.DeltaLink {
		t.Errorf("GetTeamsPollPosition = %+v, %v; want the position just saved", got, err)
	}
}

// TestTeamsPollPositionLookupUsesThePrimaryKey: the read of one channel's position is the primary key's.
func TestTeamsPollPositionLookupUsesThePrimaryKey(t *testing.T) {
	s := openTemp(t)
	plan := strings.Join(queryPlan(t, s, `SELECT delta_link FROM teams_poll_positions WHERE connection = ? AND channel = ?`, []any{"c", "19:c"}), "\n")
	if !strings.Contains(plan, "sqlite_autoindex_teams_poll_positions_1") {
		t.Errorf("query plan = %q; want it to use the primary key, sqlite_autoindex_teams_poll_positions_1", plan)
	}
}
