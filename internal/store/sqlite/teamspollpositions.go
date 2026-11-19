package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.TeamsPollPositions = (*adapter)(nil)

// SaveTeamsPollPosition implements store.TeamsPollPositions. No error names the deltaLink, which carries Graph's state token.
func (s *adapter) SaveTeamsPollPosition(ctx context.Context, p store.TeamsPollPosition) error {
	_, err := s.exec(ctx, `INSERT INTO teams_poll_positions (connection, channel, delta_link, polled_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (connection, channel) DO UPDATE SET delta_link = excluded.delta_link, polled_at = excluded.polled_at`,
		p.Connection, p.Channel, p.DeltaLink, ts(p.PolledAt))
	if err != nil {
		return fmt.Errorf("save the teams poll position of %s of %s: %w", p.Channel, p.Connection, err)
	}
	return nil
}

// GetTeamsPollPosition implements store.TeamsPollPositions.
func (s *adapter) GetTeamsPollPosition(ctx context.Context, connection, channel string) (store.TeamsPollPosition, error) {
	p := store.TeamsPollPosition{Connection: connection, Channel: channel}
	var at int64
	err := s.read.QueryRowContext(ctx, `SELECT delta_link, polled_at FROM teams_poll_positions WHERE connection = ? AND channel = ?`,
		connection, channel).Scan(&p.DeltaLink, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return store.TeamsPollPosition{}, fmt.Errorf("get the teams poll position of %s of %s: %w", channel, connection, store.ErrNotFound)
	} else if err != nil {
		return store.TeamsPollPosition{}, fmt.Errorf("get the teams poll position of %s of %s: %w", channel, connection, err)
	}
	p.PolledAt = fromTS(at)
	return p, nil
}

// DeleteTeamsPollPosition implements store.TeamsPollPositions.
func (s *adapter) DeleteTeamsPollPosition(ctx context.Context, connection, channel string) error {
	if _, err := s.exec(ctx, `DELETE FROM teams_poll_positions WHERE connection = ? AND channel = ?`, connection, channel); err != nil {
		return fmt.Errorf("delete the teams poll position of %s of %s: %w", channel, connection, err)
	}
	return nil
}

// DeleteTeamsPollPositions implements store.TeamsPollPositions.
func (s *adapter) DeleteTeamsPollPositions(ctx context.Context, connection string) error {
	if _, err := s.exec(ctx, `DELETE FROM teams_poll_positions WHERE connection = ?`, connection); err != nil {
		return fmt.Errorf("delete the teams poll positions of %s: %w", connection, err)
	}
	return nil
}

// DeleteTeamsPollPositionsExcept implements store.TeamsPollPositions. keep goes as one JSON array, read by json_each, so that a list of
// any length is one statement of two variables.
func (s *adapter) DeleteTeamsPollPositionsExcept(ctx context.Context, connection string, keep []string) error {
	if keep == nil {
		keep = []string{}
	}
	list, err := json.Marshal(keep)
	if err != nil {
		return fmt.Errorf("delete the teams poll positions of %s but %d channels: %w", connection, len(keep), err)
	}
	if _, err := s.exec(ctx, `DELETE FROM teams_poll_positions WHERE connection = ? AND channel NOT IN (SELECT value FROM json_each(?))`,
		connection, string(list)); err != nil {
		return fmt.Errorf("delete the teams poll positions of %s but %d channels: %w", connection, len(keep), err)
	}
	return nil
}
