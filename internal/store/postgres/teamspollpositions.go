package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.TeamsPollPositions = (*adapter)(nil)

// SaveTeamsPollPosition implements store.TeamsPollPositions. It is a write transaction, so it carries the epoch check of every write. No
// error names the deltaLink, which carries Graph's state token.
func (s *adapter) SaveTeamsPollPosition(ctx context.Context, p store.TeamsPollPosition) error {
	_, err := s.exec(ctx, `INSERT INTO teams_poll_positions (connection, channel, delta_link, polled_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (connection, channel) DO UPDATE SET delta_link = excluded.delta_link, polled_at = excluded.polled_at`,
		p.Connection, p.Channel, p.DeltaLink, p.PolledAt)
	if err != nil {
		return fmt.Errorf("save the teams poll position of %s of %s: %w", p.Channel, p.Connection, err)
	}
	return nil
}

// GetTeamsPollPosition implements store.TeamsPollPositions.
func (s *adapter) GetTeamsPollPosition(ctx context.Context, connection, channel string) (store.TeamsPollPosition, error) {
	p := store.TeamsPollPosition{Connection: connection, Channel: channel}
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT delta_link, polled_at FROM teams_poll_positions WHERE connection = $1 AND channel = $2`,
		connection, channel).Scan(&p.DeltaLink, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TeamsPollPosition{}, fmt.Errorf("get the teams poll position of %s of %s: %w", channel, connection, store.ErrNotFound)
	} else if err != nil {
		return store.TeamsPollPosition{}, fmt.Errorf("get the teams poll position of %s of %s: %w", channel, connection, err)
	}
	p.PolledAt = utc(at)
	return p, nil
}

// DeleteTeamsPollPosition implements store.TeamsPollPositions. It is a write transaction, so it carries the epoch check of every write.
func (s *adapter) DeleteTeamsPollPosition(ctx context.Context, connection, channel string) error {
	if _, err := s.exec(ctx, `DELETE FROM teams_poll_positions WHERE connection = $1 AND channel = $2`, connection, channel); err != nil {
		return fmt.Errorf("delete the teams poll position of %s of %s: %w", channel, connection, err)
	}
	return nil
}

// DeleteTeamsPollPositions implements store.TeamsPollPositions. It is a write transaction, so it carries the epoch check of every write.
func (s *adapter) DeleteTeamsPollPositions(ctx context.Context, connection string) error {
	if _, err := s.exec(ctx, `DELETE FROM teams_poll_positions WHERE connection = $1`, connection); err != nil {
		return fmt.Errorf("delete the teams poll positions of %s: %w", connection, err)
	}
	return nil
}

// DeleteTeamsPollPositionsExcept implements store.TeamsPollPositions. It is a write transaction, so it carries the epoch check of every
// write. keep goes as one array; an empty one is sent as '{}', not NULL, against which no channel would compare.
func (s *adapter) DeleteTeamsPollPositionsExcept(ctx context.Context, connection string, keep []string) error {
	if keep == nil {
		keep = []string{}
	}
	if _, err := s.exec(ctx, `DELETE FROM teams_poll_positions WHERE connection = $1 AND channel <> ALL($2::text[])`, connection, keep); err != nil {
		return fmt.Errorf("delete the teams poll positions of %s but %d channels: %w", connection, len(keep), err)
	}
	return nil
}
