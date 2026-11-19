package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestMigration0032AddsTheTeamsPollPositionsTableAndKeepsWhatWasThere: a database at 0031 gains an empty teams_poll_positions, its
// rows are as they were, and the table takes a position.
func TestMigration0032AddsTheTeamsPollPositionsTableAndKeepsWhatWasThere(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	migrateUpTo(t, p, 31)
	if _, err := p.Exec(ctx, `INSERT INTO teams_conversations (connection, channel, service_url, team_id, aad_group_id, tenant_id, updated_at)
		VALUES ('ns/teams', '19:c@thread.tacv2', 'https://smba.example.test/', '', '', '', $1)`, t0); err != nil {
		p.Close()
		t.Fatalf("insert a row of the old schema: %v", err)
	}
	p.Close()

	s := openAt(t, dsn)
	if got, err := s.GetTeamsConversation(ctx, "ns/teams", "19:c@thread.tacv2"); err != nil || got.ServiceURL != "https://smba.example.test/" {
		t.Fatalf("GetTeamsConversation after 0032 = %+v, %v; want the row written before it", got, err)
	}
	if _, err := s.GetTeamsPollPosition(ctx, "ns/teams", "19:c@thread.tacv2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTeamsPollPosition after 0032 = %v; want store.ErrNotFound from an empty table", err)
	}
	pos := store.TeamsPollPosition{Connection: "ns/teams", Channel: "19:c@thread.tacv2", DeltaLink: "https://graph.example.test/delta?$deltatoken=1", PolledAt: t0}
	if err := s.SaveTeamsPollPosition(ctx, pos); err != nil {
		t.Fatalf("SaveTeamsPollPosition after 0032: %v", err)
	}
	if got, err := s.GetTeamsPollPosition(ctx, pos.Connection, pos.Channel); err != nil || got.DeltaLink != pos.DeltaLink {
		t.Errorf("GetTeamsPollPosition = %+v, %v; want the position just saved", got, err)
	}
}

// TestTeamsPollPositionWritesAreFenced: the save and both deletes of a standby, and of a leader another replica has taken over from, change
// nothing, like every write, while the leader's position stands and every store can read it; the leader's own writes take effect.
func TestTeamsPollPositionWritesAreFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	first := store.TeamsPollPosition{Connection: "ns/teams", Channel: "19:c@thread.tacv2", DeltaLink: "https://graph.example.test/delta?$deltatoken=1", PolledAt: t0}
	if err := a.SaveTeamsPollPosition(ctx, first); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("SaveTeamsPollPosition of a standby = %v; want store.ErrDeposed", err)
	}
	if _, err := a.GetTeamsPollPosition(ctx, first.Connection, first.Channel); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTeamsPollPosition = %v; want store.ErrNotFound: the standby's write changed nothing", err)
	}

	if _, err := a.Lead(ctx, "pod-a"); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveTeamsPollPosition(ctx, first); err != nil {
		t.Fatalf("the leader's SaveTeamsPollPosition: %v", err)
	}
	for what, write := range map[string]func() error{
		"DeleteTeamsPollPosition of a standby":  func() error { return b.DeleteTeamsPollPosition(ctx, first.Connection, first.Channel) },
		"DeleteTeamsPollPositions of a standby": func() error { return b.DeleteTeamsPollPositions(ctx, first.Connection) },
		"DeleteTeamsPollPositionsExcept of a standby": func() error {
			return b.DeleteTeamsPollPositionsExcept(ctx, first.Connection, nil)
		},
	} {
		if err := write(); !errors.Is(err, store.ErrDeposed) {
			t.Fatalf("%s = %v; want store.ErrDeposed", what, err)
		}
	}
	if _, err := b.Lead(ctx, "pod-b"); err != nil {
		t.Fatal(err)
	}
	moved := first
	moved.DeltaLink, moved.PolledAt = first.DeltaLink+"2", t0.Add(time.Minute)
	for what, write := range map[string]func() error{
		"SaveTeamsPollPosition of a deposed leader":    func() error { return a.SaveTeamsPollPosition(ctx, moved) },
		"DeleteTeamsPollPosition of a deposed leader":  func() error { return a.DeleteTeamsPollPosition(ctx, first.Connection, first.Channel) },
		"DeleteTeamsPollPositions of a deposed leader": func() error { return a.DeleteTeamsPollPositions(ctx, first.Connection) },
		"DeleteTeamsPollPositionsExcept of a deposed leader": func() error {
			return a.DeleteTeamsPollPositionsExcept(ctx, first.Connection, nil)
		},
	} {
		if err := write(); !errors.Is(err, store.ErrDeposed) {
			t.Fatalf("%s = %v; want store.ErrDeposed", what, err)
		}
	}
	for name, s := range map[string]*Store{"the deposed leader": a, "the new leader": b} {
		if got, err := s.GetTeamsPollPosition(ctx, first.Connection, first.Channel); err != nil || got.DeltaLink != first.DeltaLink || !got.PolledAt.Equal(t0) {
			t.Errorf("GetTeamsPollPosition from %s = %+v, %v; want the first leader's position, unchanged", name, got, err)
		}
	}
	if err := b.SaveTeamsPollPosition(ctx, moved); err != nil {
		t.Fatalf("the new leader's SaveTeamsPollPosition: %v", err)
	}
	if err := b.DeleteTeamsPollPosition(ctx, first.Connection, first.Channel); err != nil {
		t.Fatalf("the new leader's DeleteTeamsPollPosition: %v", err)
	}
	if _, err := b.GetTeamsPollPosition(ctx, first.Connection, first.Channel); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTeamsPollPosition after the leader's delete = %v; want store.ErrNotFound", err)
	}
}
