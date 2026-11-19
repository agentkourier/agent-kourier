package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// teamsPollPositions is the teams.PollPositions of the Microsoft Teams pollers over the store, so that where the poll of each channel
// stands outlives a restart and moves with the leader. Only the leader polls, and the store refuses a write of a replica that is not the
// leader (store.ErrDeposed): Save and Forget then wrap teams.ErrNotLeader, and the poller stops. On Postgres a write that finds another
// replica has led since also ends the process (spec D37). The leader deletes a connection's positions when its ChatConnection is deleted
// and its run has stopped (servedRecord).
type teamsPollPositions struct {
	s store.TeamsPollPositions
}

var _ teams.PollPositions = teamsPollPositions{}

// newTeamsPollPositions returns the teams.PollPositions over s, the broker's store.
func newTeamsPollPositions(s store.TeamsPollPositions) teamsPollPositions {
	return teamsPollPositions{s: s}
}

// Load implements teams.PollPositions.
func (t teamsPollPositions) Load(ctx context.Context, conn, channel string) (teams.PollPosition, bool, error) {
	p, err := t.s.GetTeamsPollPosition(ctx, conn, channel)
	if errors.Is(err, store.ErrNotFound) {
		return teams.PollPosition{}, false, nil
	}
	if err != nil {
		return teams.PollPosition{}, false, fmt.Errorf("teams poll positions: %w", err)
	}
	return teams.PollPosition{Channel: p.Channel, DeltaLink: p.DeltaLink, PolledAt: p.PolledAt}, true, nil
}

// Save implements teams.PollPositions.
func (t teamsPollPositions) Save(ctx context.Context, conn string, p teams.PollPosition) error {
	return written(t.s.SaveTeamsPollPosition(ctx, store.TeamsPollPosition{Connection: conn, Channel: p.Channel, DeltaLink: p.DeltaLink,
		PolledAt: p.PolledAt}))
}

// Forget implements teams.PollPositions.
func (t teamsPollPositions) Forget(ctx context.Context, conn, channel string) error {
	return written(t.s.DeleteTeamsPollPosition(ctx, conn, channel))
}

// written is the error of a write of the positions: store.ErrDeposed wraps teams.ErrNotLeader too, so that the poller stops.
func written(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrDeposed):
		return fmt.Errorf("teams poll positions: %w: %w", teams.ErrNotLeader, err)
	default:
		return fmt.Errorf("teams poll positions: %w", err)
	}
}
