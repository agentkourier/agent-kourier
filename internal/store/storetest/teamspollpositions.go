package storetest

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func (su suite) teamsPollPositions(t *testing.T) {
	const (
		conn    = "ns/teams"
		channel = "19:general@thread.tacv2"
		link    = "https://graph.microsoft.com/v1.0/teams/g/channels/19:general@thread.tacv2/messages/delta?$deltatoken=stand-in-token-1"
	)
	pos := store.TeamsPollPosition{Connection: conn, Channel: channel, DeltaLink: link, PolledAt: t0}
	save := func(t *testing.T, s store.Store, p store.TeamsPollPosition) {
		t.Helper()
		noErr(t, s.SaveTeamsPollPosition(t.Context(), p), "SaveTeamsPollPosition")
	}
	// expect fails the test unless the position of want's connection and channel is want, its time the same instant in UTC.
	expect := func(t *testing.T, s store.Store, want store.TeamsPollPosition) {
		t.Helper()
		got, err := s.GetTeamsPollPosition(t.Context(), want.Connection, want.Channel)
		noErr(t, err, "GetTeamsPollPosition")
		if !got.PolledAt.Equal(want.PolledAt) || got.PolledAt.Location() != time.UTC {
			t.Errorf("PolledAt = %v; want the instant %v in UTC", got.PolledAt, want.PolledAt)
		}
		got.PolledAt, want.PolledAt = time.Time{}, time.Time{}
		if got != want {
			t.Errorf("GetTeamsPollPosition = %+v; want %+v", got, want)
		}
	}
	gone := func(t *testing.T, s store.Store, connection, channel, what string) {
		t.Helper()
		if _, err := s.GetTeamsPollPosition(t.Context(), connection, channel); !isNotFound(err) {
			t.Errorf("GetTeamsPollPosition(%q, %q) %s = %v; want store.ErrNotFound", connection, channel, what, err)
		}
	}

	su.runReopen(t, "a channel with no position is not found, and a position survives a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		gone(t, s, conn, channel, "of a new store")
		save(t, s, pos)
		expect(t, s, pos)
		expect(t, reopen(), pos)
	})

	su.run(t, "SaveTeamsPollPosition replaces the channel's position, link and time", func(t *testing.T, s store.Store) {
		save(t, s, pos)
		next := pos
		next.DeltaLink, next.PolledAt = link+"-2", t0.Add(time.Minute)
		save(t, s, next)
		expect(t, s, next)
		// The same link at a later poll moves the time alone.
		again := next
		again.PolledAt = t0.Add(10 * time.Minute)
		save(t, s, again)
		expect(t, s, again)
	})

	su.run(t, "positions are kept apart by connection and by channel, byte for byte", func(t *testing.T, s store.Store) {
		other := pos
		other.Connection, other.DeltaLink = "ns/other", link+"-other"
		second := pos
		second.Channel, second.DeltaLink = "19:second@thread.tacv2", link+"-second"
		for _, p := range []store.TeamsPollPosition{pos, other, second} {
			save(t, s, p)
		}
		for _, p := range []store.TeamsPollPosition{pos, other, second} {
			expect(t, s, p)
		}
		for _, k := range [][2]string{{"ns/other", second.Channel}, {"NS/teams", channel}, {conn, "19:GENERAL@thread.tacv2"}, {conn + " ", channel}} {
			gone(t, s, k[0], k[1], "of a key no position has")
		}
	})

	su.run(t, "SaveTeamsPollPosition with no connection, channel, link or time is refused and changes nothing", func(t *testing.T, s store.Store) {
		save(t, s, pos)
		for what, p := range map[string]store.TeamsPollPosition{
			"no connection":    {Channel: channel, DeltaLink: link + "-x", PolledAt: t0},
			"no channel":       {Connection: conn, DeltaLink: link + "-x", PolledAt: t0},
			"no link":          {Connection: conn, Channel: channel, PolledAt: t0},
			"no time":          {Connection: conn, Channel: channel, DeltaLink: link + "-x"},
			"a time at epoch":  {Connection: conn, Channel: channel, DeltaLink: link + "-x", PolledAt: time.Unix(0, 0)},
			"a time before it": {Connection: conn, Channel: channel, DeltaLink: link + "-x", PolledAt: time.Unix(-1, 0)},
		} {
			isErr(t, s.SaveTeamsPollPosition(t.Context(), p), store.ErrInvalid, what)
		}
		expect(t, s, pos)
		_, err := s.GetTeamsPollPosition(t.Context(), "", channel)
		isErr(t, err, store.ErrInvalid, "GetTeamsPollPosition with no connection")
		_, err = s.GetTeamsPollPosition(t.Context(), conn, "")
		isErr(t, err, store.ErrInvalid, "GetTeamsPollPosition with no channel")
		isErr(t, s.DeleteTeamsPollPosition(t.Context(), "", channel), store.ErrInvalid, "DeleteTeamsPollPosition with no connection")
		isErr(t, s.DeleteTeamsPollPosition(t.Context(), conn, ""), store.ErrInvalid, "DeleteTeamsPollPosition with no channel")
		isErr(t, s.DeleteTeamsPollPositions(t.Context(), ""), store.ErrInvalid, "DeleteTeamsPollPositions with no connection")
		isErr(t, s.DeleteTeamsPollPositionsExcept(t.Context(), "", []string{channel}), store.ErrInvalid,
			"DeleteTeamsPollPositionsExcept with no connection")
		isErr(t, s.DeleteTeamsPollPositionsExcept(t.Context(), conn, []string{"19:other@thread.tacv2", ""}), store.ErrInvalid,
			"DeleteTeamsPollPositionsExcept with an empty channel to keep")
		expect(t, s, pos)
	})

	su.runReopen(t, "DeleteTeamsPollPosition deletes the channel's position and no other, for good", func(t *testing.T, s store.Store, reopen func() store.Store) {
		second := pos
		second.Channel = "19:second@thread.tacv2"
		other := pos
		other.Connection = "ns/other"
		for _, p := range []store.TeamsPollPosition{pos, second, other} {
			save(t, s, p)
		}
		noErr(t, s.DeleteTeamsPollPosition(t.Context(), conn, channel), "DeleteTeamsPollPosition")
		noErr(t, s.DeleteTeamsPollPosition(t.Context(), conn, channel), "DeleteTeamsPollPosition of a channel with none")
		s = reopen()
		gone(t, s, conn, channel, "after the delete")
		expect(t, s, second)
		expect(t, s, other)
		// A position saved after the delete is the new one.
		fresh := pos
		fresh.DeltaLink, fresh.PolledAt = link+"-fresh", t0.Add(time.Hour)
		save(t, s, fresh)
		expect(t, s, fresh)
	})

	su.runReopen(t, "DeleteTeamsPollPositions deletes every position of the connection and no other, for good", func(t *testing.T, s store.Store, reopen func() store.Store) {
		second := pos
		second.Channel = "19:second@thread.tacv2"
		other := pos
		other.Connection = "ns/other"
		lookalike := pos
		lookalike.Connection = "NS/teams"
		for _, p := range []store.TeamsPollPosition{pos, second, other, lookalike} {
			save(t, s, p)
		}
		noErr(t, s.DeleteTeamsPollPositions(t.Context(), conn), "DeleteTeamsPollPositions")
		noErr(t, s.DeleteTeamsPollPositions(t.Context(), "ns/none"), "DeleteTeamsPollPositions of a connection with none")
		s = reopen()
		gone(t, s, conn, pos.Channel, "after the delete")
		gone(t, s, conn, second.Channel, "after the delete")
		expect(t, s, other)
		expect(t, s, lookalike)
	})

	su.runReopen(t, "DeleteTeamsPollPositionsExcept deletes the connection's positions of the channels not kept, and no other, for good", func(t *testing.T, s store.Store, reopen func() store.Store) {
		kept := pos
		kept.Channel = "19:kept@thread.tacv2"
		lookalike := pos
		lookalike.Channel = "19:KEPT@thread.tacv2"
		other := pos
		other.Connection = "ns/other"
		for _, p := range []store.TeamsPollPosition{pos, kept, lookalike, other} {
			save(t, s, p)
		}
		noErr(t, s.DeleteTeamsPollPositionsExcept(t.Context(), conn, []string{kept.Channel, "19:unstored@thread.tacv2"}),
			"DeleteTeamsPollPositionsExcept")
		s = reopen()
		gone(t, s, conn, pos.Channel, "of a channel not kept")
		gone(t, s, conn, lookalike.Channel, "of a channel kept only in another case")
		expect(t, s, kept)
		expect(t, s, other)
	})

	su.run(t, "DeleteTeamsPollPositionsExcept with nothing to keep deletes every position of the connection, and no other", func(t *testing.T, s store.Store) {
		second := pos
		second.Channel = "19:second@thread.tacv2"
		other := pos
		other.Connection = "ns/other"
		for _, p := range []store.TeamsPollPosition{pos, second, other} {
			save(t, s, p)
		}
		noErr(t, s.DeleteTeamsPollPositionsExcept(t.Context(), conn, []string{}), "DeleteTeamsPollPositionsExcept of an empty list")
		gone(t, s, conn, pos.Channel, "after the delete")
		gone(t, s, conn, second.Channel, "after the delete")
		expect(t, s, other)
		save(t, s, pos)
		noErr(t, s.DeleteTeamsPollPositionsExcept(t.Context(), conn, nil), "DeleteTeamsPollPositionsExcept of nil")
		gone(t, s, conn, pos.Channel, "after the delete of nil")
		expect(t, s, other)
	})
}
