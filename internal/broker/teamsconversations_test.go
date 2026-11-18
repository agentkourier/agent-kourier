package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

var teamsConv = teams.Conversation{
	Channel:    "19:general@thread.tacv2",
	ServiceURL: "https://smba.example.test/amer/tenant/",
	TeamID:     "19:team@thread.tacv2",
	AADGroupID: "00000000-0000-0000-0000-00000000000a",
	TenantID:   "00000000-0000-0000-0000-00000000000b",
}

// TestTeamsConversationStoresAgree holds the store-backed teams.ConversationStore and teams.MemoryConversations to the same
// behaviour: what is not kept is not found, Remember merges, and connections and channels are kept apart.
func TestTeamsConversationStoresAgree(t *testing.T) {
	impls := map[string]func(t *testing.T) teams.ConversationStore{
		"memory": func(*testing.T) teams.ConversationStore { return teams.NewMemoryConversations() },
		"store":  func(t *testing.T) teams.ConversationStore { return newTeamsConversations(sqlitetest.Open(t)) },
	}
	cases := []struct {
		name string
		run  func(t *testing.T, cs teams.ConversationStore)
	}{
		{"a channel with nothing kept is not found", func(t *testing.T, cs teams.ConversationStore) {
			wantTeamsLookup(t, cs, "ns/teams", teamsConv.Channel, teams.Conversation{}, false)
		}},
		{"a remembered channel is found as it was given", func(t *testing.T, cs teams.ConversationStore) {
			rememberTeams(t, cs, "ns/teams", teamsConv)
			wantTeamsLookup(t, cs, "ns/teams", teamsConv.Channel, teamsConv, true)
		}},
		{"a field left empty keeps its value and one set replaces it", func(t *testing.T, cs teams.ConversationStore) {
			rememberTeams(t, cs, "ns/teams", teams.Conversation{Channel: teamsConv.Channel, ServiceURL: teamsConv.ServiceURL})
			rememberTeams(t, cs, "ns/teams", teams.Conversation{Channel: teamsConv.Channel, TeamID: teamsConv.TeamID, AADGroupID: teamsConv.AADGroupID, TenantID: teamsConv.TenantID})
			wantTeamsLookup(t, cs, "ns/teams", teamsConv.Channel, teamsConv, true)
			moved := teamsConv
			moved.ServiceURL = "https://smba.example.test/emea/tenant/"
			rememberTeams(t, cs, "ns/teams", teams.Conversation{Channel: teamsConv.Channel, ServiceURL: moved.ServiceURL})
			wantTeamsLookup(t, cs, "ns/teams", teamsConv.Channel, moved, true)
		}},
		{"connections and channels are kept apart", func(t *testing.T, cs teams.ConversationStore) {
			other := teamsConv
			other.ServiceURL = "https://smba.example.test/apac/other/"
			rememberTeams(t, cs, "ns/teams", teamsConv)
			rememberTeams(t, cs, "ns/other", other)
			wantTeamsLookup(t, cs, "ns/teams", teamsConv.Channel, teamsConv, true)
			wantTeamsLookup(t, cs, "ns/other", teamsConv.Channel, other, true)
			wantTeamsLookup(t, cs, "ns/third", teamsConv.Channel, teams.Conversation{}, false)
			wantTeamsLookup(t, cs, "ns/teams", "19:another@thread.tacv2", teams.Conversation{}, false)
		}},
		{"a record with no channel is refused and nothing is kept", func(t *testing.T, cs teams.ConversationStore) {
			if err := cs.Remember(t.Context(), "ns/teams", teams.Conversation{ServiceURL: teamsConv.ServiceURL}); err == nil {
				t.Fatal("Remember of a record with no channel = nil; want an error")
			}
			wantTeamsLookup(t, cs, "ns/teams", teamsConv.Channel, teams.Conversation{}, false)
		}},
	}
	for name, open := range impls {
		for _, c := range cases {
			t.Run(name+"/"+c.name, func(t *testing.T) { c.run(t, open(t)) })
		}
	}
}

// TestTeamsConversationsSurviveARestart: what the store-backed store remembers, a store opened again over the same database finds,
// and the record's time is the adapter's clock.
func TestTeamsConversationsSurviveARestart(t *testing.T) {
	path := sqlitetest.Path(t)
	st, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	cs := newTeamsConversations(st)
	cs.now = func() time.Time { return at }
	rememberTeams(t, cs, "ns/teams", teamsConv)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	wantTeamsLookup(t, newTeamsConversations(again), "ns/teams", teamsConv.Channel, teamsConv, true)
	rec, err := again.GetTeamsConversation(t.Context(), "ns/teams", teamsConv.Channel)
	if err != nil || !rec.UpdatedAt.Equal(at) {
		t.Errorf("the stored record = %+v, %v; want it updated at the adapter's clock, %v", rec, err, at)
	}
}

// failingTeamsStore is a store.TeamsConversations whose every call fails with err.
type failingTeamsStore struct{ err error }

func (f failingTeamsStore) RememberTeamsConversation(context.Context, store.TeamsConversation) error {
	return f.err
}

func (f failingTeamsStore) GetTeamsConversation(context.Context, string, string) (store.TeamsConversation, error) {
	return store.TeamsConversation{}, f.err
}

func (f failingTeamsStore) DeleteTeamsConversations(context.Context, string) error { return f.err }

// TestTeamsConversationsPassTheStoresErrorsOn: a write the store refuses, a deposed leader's among them, fails the call with the
// store's error, and so does a read that fails for any reason but a missing record.
func TestTeamsConversationsPassTheStoresErrorsOn(t *testing.T) {
	cs := newTeamsConversations(failingTeamsStore{err: store.ErrDeposed})
	if err := cs.Remember(t.Context(), "ns/teams", teamsConv); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("Remember = %v; want store.ErrDeposed", err)
	}
	broken := errors.New("disk on fire")
	cs = newTeamsConversations(failingTeamsStore{err: broken})
	if got, ok, err := cs.Lookup(t.Context(), "ns/teams", teamsConv.Channel); !errors.Is(err, broken) || ok || got != (teams.Conversation{}) {
		t.Errorf("Lookup = %+v, %v, %v; want nothing, false and the store's error", got, ok, err)
	}
}

func rememberTeams(t *testing.T, cs teams.ConversationStore, conn string, c teams.Conversation) {
	t.Helper()
	if err := cs.Remember(t.Context(), conn, c); err != nil {
		t.Fatalf("Remember(%s, %s): %v", conn, c.Channel, err)
	}
}

func wantTeamsLookup(t *testing.T, cs teams.ConversationStore, conn, channel string, want teams.Conversation, wantOK bool) {
	t.Helper()
	got, ok, err := cs.Lookup(t.Context(), conn, channel)
	if err != nil || ok != wantOK || got != want {
		t.Errorf("Lookup(%s, %s) = %+v, %v, %v; want %+v, %v, nil", conn, channel, got, ok, err, want, wantOK)
	}
}
