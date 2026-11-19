package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// The store-backed teams.PollPositions: a channel with none is not found, a saved position is loaded as it was given and replaced by the
// next, Forget deletes one channel's, ForgetUnbound those of the channels not bound, and connections and channels are kept apart.
func TestTeamsPollPositionsOverTheStore(t *testing.T) {
	ctx := t.Context()
	const (
		conn  = "ns/teams"
		other = "19:other@thread.tacv2"
	)
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	pp := newTeamsPollPositions(sqlitetest.Open(t))
	want := teams.PollPosition{Channel: "19:general@thread.tacv2", DeltaLink: "https://graph.example.test/delta?$deltatoken=1", PolledAt: at}
	load := func(conn, channel string) (teams.PollPosition, bool) {
		t.Helper()
		p, ok, err := pp.Load(ctx, conn, channel)
		if err != nil {
			t.Fatalf("Load(%s, %s): %v", conn, channel, err)
		}
		return p, ok
	}
	if _, ok := load(conn, want.Channel); ok {
		t.Fatal("Load of a channel with no position found one")
	}
	for _, p := range []teams.PollPosition{want, {Channel: other, DeltaLink: want.DeltaLink + "-other", PolledAt: at}} {
		if err := pp.Save(ctx, conn, p); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := load(conn, want.Channel); !ok || got != want {
		t.Errorf("Load = %+v, %v; want %+v", got, ok, want)
	}
	next := want
	next.DeltaLink, next.PolledAt = want.DeltaLink+"-2", at.Add(time.Minute)
	if err := pp.Save(ctx, conn, next); err != nil {
		t.Fatal(err)
	}
	if got, ok := load(conn, want.Channel); !ok || got != next {
		t.Errorf("Load after a second Save = %+v, %v; want %+v", got, ok, next)
	}
	if _, ok := load("ns/elsewhere", want.Channel); ok {
		t.Error("Load of another connection found the position")
	}
	if err := pp.Forget(ctx, conn, want.Channel); err != nil {
		t.Fatal(err)
	}
	if _, ok := load(conn, want.Channel); ok {
		t.Error("Load after Forget found the position")
	}
	if _, ok := load(conn, other); !ok {
		t.Error("Forget of one channel deleted another's")
	}
	third := teams.PollPosition{Channel: "19:third@thread.tacv2", DeltaLink: want.DeltaLink + "-third", PolledAt: at}
	for _, s := range []struct {
		conn string
		p    teams.PollPosition
	}{{conn, third}, {"ns/elsewhere", third}} {
		if err := pp.Save(ctx, s.conn, s.p); err != nil {
			t.Fatal(err)
		}
	}
	if err := pp.ForgetUnbound(ctx, conn, []string{third.Channel}); err != nil {
		t.Fatal(err)
	}
	if _, ok := load(conn, other); ok {
		t.Error("Load after ForgetUnbound found the position of a channel not bound")
	}
	if _, ok := load(conn, third.Channel); !ok {
		t.Error("ForgetUnbound deleted the position of a bound channel")
	}
	if _, ok := load("ns/elsewhere", third.Channel); !ok {
		t.Error("ForgetUnbound deleted a position of another connection")
	}
}

// refusingPositions is a store.TeamsPollPositions whose every call fails with err.
type refusingPositions struct{ err error }

func (r refusingPositions) SaveTeamsPollPosition(context.Context, store.TeamsPollPosition) error {
	return r.err
}
func (r refusingPositions) GetTeamsPollPosition(context.Context, string, string) (store.TeamsPollPosition, error) {
	return store.TeamsPollPosition{}, r.err
}
func (r refusingPositions) DeleteTeamsPollPosition(context.Context, string, string) error {
	return r.err
}
func (r refusingPositions) DeleteTeamsPollPositions(context.Context, string) error { return r.err }
func (r refusingPositions) DeleteTeamsPollPositionsExcept(context.Context, string, []string) error {
	return r.err
}

// A write that the store refuses because this broker no longer leads (store.ErrDeposed) is teams.ErrNotLeader to the poller, which then
// stops; another failure is not, and a read's failure is returned as it is.
func TestTeamsPollPositionsTellThePollerItNoLongerLeads(t *testing.T) {
	ctx := t.Context()
	p := teams.PollPosition{Channel: "19:general@thread.tacv2", DeltaLink: "https://graph.example.test/delta", PolledAt: time.Now()}
	deposed := newTeamsPollPositions(refusingPositions{err: store.ErrDeposed})
	for what, err := range map[string]error{"Save": deposed.Save(ctx, "ns/teams", p), "Forget": deposed.Forget(ctx, "ns/teams", p.Channel),
		"ForgetUnbound": deposed.ForgetUnbound(ctx, "ns/teams", nil)} {
		if !errors.Is(err, teams.ErrNotLeader) || !errors.Is(err, store.ErrDeposed) {
			t.Errorf("%s of a deposed store = %v; want teams.ErrNotLeader and store.ErrDeposed", what, err)
		}
	}
	down := errors.New("the store is down")
	failing := newTeamsPollPositions(refusingPositions{err: down})
	for what, err := range map[string]error{"Save": failing.Save(ctx, "ns/teams", p), "Forget": failing.Forget(ctx, "ns/teams", p.Channel),
		"ForgetUnbound": failing.ForgetUnbound(ctx, "ns/teams", nil)} {
		if errors.Is(err, teams.ErrNotLeader) || !errors.Is(err, down) {
			t.Errorf("%s of a failing store = %v; want its error, and not teams.ErrNotLeader", what, err)
		}
	}
	if _, ok, err := failing.Load(ctx, "ns/teams", p.Channel); ok || !errors.Is(err, down) {
		t.Errorf("Load of a failing store = %v, %v; want its error", ok, err)
	}
}
