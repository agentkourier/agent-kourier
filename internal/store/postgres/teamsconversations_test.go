package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestMigration0031AddsTheTeamsConversationsTableAndKeepsWhatWasThere: a database at 0030 gains an empty
// teams_conversations, its rows are as they were, and the table takes a record.
func TestMigration0031AddsTheTeamsConversationsTableAndKeepsWhatWasThere(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	migrateUpTo(t, p, 30)
	if _, err := p.Exec(ctx, `INSERT INTO served_connections (connection, served_since) VALUES ('ns/teams', $1)`, t0); err != nil {
		p.Close()
		t.Fatalf("insert a row of the old schema: %v", err)
	}
	p.Close()

	s := openAt(t, dsn)
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

// TestRememberTeamsConversationIsFenced: a standby, and a leader another replica has taken over from, change nothing, like every
// write, while the leader's record stands and every store can read it.
func TestRememberTeamsConversationIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	first := store.TeamsConversation{Connection: "ns/teams", Channel: "19:c@thread.tacv2", ServiceURL: "https://smba.example.test/amer/", UpdatedAt: t0}
	if err := a.RememberTeamsConversation(ctx, first); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("RememberTeamsConversation of a standby = %v; want store.ErrDeposed", err)
	}
	if _, err := a.GetTeamsConversation(ctx, first.Connection, first.Channel); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTeamsConversation = %v; want store.ErrNotFound: the standby's write changed nothing", err)
	}

	if _, err := a.Lead(ctx, "pod-a"); err != nil {
		t.Fatal(err)
	}
	if err := a.RememberTeamsConversation(ctx, first); err != nil {
		t.Fatalf("the leader's RememberTeamsConversation: %v", err)
	}
	if _, err := b.Lead(ctx, "pod-b"); err != nil {
		t.Fatal(err)
	}
	moved := first
	moved.ServiceURL, moved.UpdatedAt = "https://smba.example.test/emea/", t0.Add(time.Minute)
	if err := a.RememberTeamsConversation(ctx, moved); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("RememberTeamsConversation of a deposed leader = %v; want store.ErrDeposed", err)
	}
	for name, s := range map[string]*Store{"the deposed store": a, "the new leader": b} {
		if got, err := s.GetTeamsConversation(ctx, first.Connection, first.Channel); err != nil || got.ServiceURL != first.ServiceURL || !got.UpdatedAt.Equal(t0) {
			t.Errorf("GetTeamsConversation from %s = %+v, %v; want the first leader's record, unchanged", name, got, err)
		}
	}
}

// TestDeleteTeamsConversationsIsFenced: the delete of a standby, and of a leader another replica has taken over from, changes nothing, like
// every write, and the leader's delete takes the records.
func TestDeleteTeamsConversationsIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := t.Context()
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	if _, err := a.Lead(ctx, "pod-a"); err != nil {
		t.Fatal(err)
	}
	rec := store.TeamsConversation{Connection: "ns/teams", Channel: "19:c@thread.tacv2", ServiceURL: "https://smba.example.test/amer/", UpdatedAt: t0}
	if err := a.RememberTeamsConversation(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteTeamsConversations(ctx, rec.Connection); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("DeleteTeamsConversations of a standby = %v; want store.ErrDeposed", err)
	}
	if _, err := b.Lead(ctx, "pod-b"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteTeamsConversations(ctx, rec.Connection); !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("DeleteTeamsConversations of a deposed leader = %v; want store.ErrDeposed", err)
	}
	if got, err := b.GetTeamsConversation(ctx, rec.Connection, rec.Channel); err != nil || got.ServiceURL != rec.ServiceURL {
		t.Fatalf("GetTeamsConversation = %+v, %v; want the record, which neither refused delete changed", got, err)
	}
	if err := b.DeleteTeamsConversations(ctx, rec.Connection); err != nil {
		t.Fatalf("the leader's DeleteTeamsConversations: %v", err)
	}
	if _, err := b.GetTeamsConversation(ctx, rec.Connection, rec.Channel); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTeamsConversation after the leader's delete = %v; want store.ErrNotFound", err)
	}
}
