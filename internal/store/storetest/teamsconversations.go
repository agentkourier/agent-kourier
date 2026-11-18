package storetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func (su suite) teamsConversations(t *testing.T) {
	const (
		conn    = "ns/teams"
		channel = "19:general@thread.tacv2"
	)
	full := store.TeamsConversation{
		Connection: conn, Channel: channel,
		ServiceURL: "https://smba.example.test/amer/tenant/", TeamID: "19:team@thread.tacv2",
		AADGroupID: "00000000-0000-0000-0000-00000000000a", TenantID: "00000000-0000-0000-0000-00000000000b",
		UpdatedAt: t0,
	}
	remember := func(t *testing.T, s store.Store, c store.TeamsConversation) {
		t.Helper()
		noErr(t, s.RememberTeamsConversation(t.Context(), c), "RememberTeamsConversation")
	}
	// expect fails the test unless the record of c's connection and channel is c, its time the same instant in UTC.
	expect := func(t *testing.T, s store.Store, want store.TeamsConversation) {
		t.Helper()
		got, err := s.GetTeamsConversation(t.Context(), want.Connection, want.Channel)
		noErr(t, err, "GetTeamsConversation")
		if !got.UpdatedAt.Equal(want.UpdatedAt) || got.UpdatedAt.Location() != time.UTC {
			t.Errorf("UpdatedAt = %v; want the instant %v in UTC", got.UpdatedAt, want.UpdatedAt)
		}
		got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}
		if got != want {
			t.Errorf("GetTeamsConversation = %+v; want %+v", got, want)
		}
	}

	su.runReopen(t, "a channel with no record is not found, and a record survives a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		if _, err := s.GetTeamsConversation(t.Context(), conn, channel); !isNotFound(err) {
			t.Fatalf("GetTeamsConversation of a new store = %v; want store.ErrNotFound", err)
		}
		remember(t, s, full)
		expect(t, s, full)
		expect(t, reopen(), full)
	})

	su.run(t, "RememberTeamsConversation merges: a field left empty keeps its value, one set replaces it, and the time is the call's", func(t *testing.T, s store.Store) {
		remember(t, s, full)
		moved := store.TeamsConversation{Connection: conn, Channel: channel, ServiceURL: "https://smba.example.test/emea/tenant/", UpdatedAt: t0.Add(time.Minute)}
		remember(t, s, moved)
		want := full
		want.ServiceURL, want.UpdatedAt = moved.ServiceURL, moved.UpdatedAt
		expect(t, s, want)

		// A record that names only its channel changes nothing but the time.
		remember(t, s, store.TeamsConversation{Connection: conn, Channel: channel, UpdatedAt: t0.Add(2 * time.Minute)})
		want.UpdatedAt = t0.Add(2 * time.Minute)
		expect(t, s, want)
	})

	su.run(t, "a record made from some fields has the others empty, and later records fill them in", func(t *testing.T, s store.Store) {
		first := store.TeamsConversation{Connection: conn, Channel: channel, ServiceURL: full.ServiceURL, UpdatedAt: t0}
		remember(t, s, first)
		expect(t, s, first)
		remember(t, s, store.TeamsConversation{Connection: conn, Channel: channel, TeamID: full.TeamID, AADGroupID: full.AADGroupID, UpdatedAt: t0})
		remember(t, s, store.TeamsConversation{Connection: conn, Channel: channel, TenantID: full.TenantID, UpdatedAt: t0})
		expect(t, s, full)
	})

	su.run(t, "records are kept apart by connection and by channel, byte for byte", func(t *testing.T, s store.Store) {
		other := full
		other.Connection, other.ServiceURL, other.TeamID = "ns/other", "https://smba.example.test/apac/other/", "19:otherteam@thread.tacv2"
		second := full
		second.Channel, second.ServiceURL = "19:second@thread.tacv2", "https://smba.example.test/emea/tenant/"
		for _, c := range []store.TeamsConversation{full, other, second} {
			remember(t, s, c)
		}
		for _, c := range []store.TeamsConversation{full, other, second} {
			expect(t, s, c)
		}
		for _, k := range [][2]string{{"ns/other", second.Channel}, {"NS/teams", channel}, {conn, "19:GENERAL@thread.tacv2"}, {conn + " ", channel}} {
			if _, err := s.GetTeamsConversation(t.Context(), k[0], k[1]); !isNotFound(err) {
				t.Errorf("GetTeamsConversation(%q, %q) = %v; want store.ErrNotFound", k[0], k[1], err)
			}
		}
	})

	su.run(t, "RememberTeamsConversation with no connection, channel or time is refused and changes nothing", func(t *testing.T, s store.Store) {
		remember(t, s, full)
		for what, c := range map[string]store.TeamsConversation{
			"no connection": {Channel: channel, ServiceURL: "https://x.example.test/", UpdatedAt: t0},
			"no channel":    {Connection: conn, ServiceURL: "https://x.example.test/", UpdatedAt: t0},
			"no time":       {Connection: conn, Channel: channel, ServiceURL: "https://x.example.test/"},
		} {
			isErr(t, s.RememberTeamsConversation(t.Context(), c), store.ErrInvalid, what)
		}
		expect(t, s, full)
		_, err := s.GetTeamsConversation(t.Context(), "", channel)
		isErr(t, err, store.ErrInvalid, "GetTeamsConversation with no connection")
		_, err = s.GetTeamsConversation(t.Context(), conn, "")
		isErr(t, err, store.ErrInvalid, "GetTeamsConversation with no channel")
	})

	// Records of one channel that come together each keep what they set: a store that read the record, merged and wrote it back
	// would lose one of them.
	su.run(t, "RememberTeamsConversation calls that run together for one channel each keep what they set", func(t *testing.T, s store.Store) {
		for round := range 30 {
			ch := fmt.Sprintf("19:together%d@thread.tacv2", round)
			parts := []store.TeamsConversation{
				{Connection: conn, Channel: ch, ServiceURL: full.ServiceURL, UpdatedAt: t0},
				{Connection: conn, Channel: ch, TeamID: full.TeamID, UpdatedAt: t0},
				{Connection: conn, Channel: ch, AADGroupID: full.AADGroupID, UpdatedAt: t0},
				{Connection: conn, Channel: ch, TenantID: full.TenantID, UpdatedAt: t0},
			}
			parallel(len(parts), func(i int) {
				if err := s.RememberTeamsConversation(t.Context(), parts[i]); err != nil {
					t.Errorf("RememberTeamsConversation: %v", err)
				}
			})
			want := full
			want.Channel = ch
			expect(t, s, want)
		}
	})
}
