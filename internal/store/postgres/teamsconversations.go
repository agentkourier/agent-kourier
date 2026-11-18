package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.TeamsConversations = (*adapter)(nil)

// RememberTeamsConversation implements store.TeamsConversations. The merge is one statement, so records of one channel that come
// together each keep what they set: a second insert of a new row waits for the first and then updates it. It is a write
// transaction, so it carries the epoch check of every write.
func (s *adapter) RememberTeamsConversation(ctx context.Context, c store.TeamsConversation) error {
	_, err := s.exec(ctx, `INSERT INTO teams_conversations (connection, channel, service_url, team_id, aad_group_id, tenant_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (connection, channel) DO UPDATE SET
			service_url  = COALESCE(NULLIF(excluded.service_url, ''), teams_conversations.service_url),
			team_id      = COALESCE(NULLIF(excluded.team_id, ''), teams_conversations.team_id),
			aad_group_id = COALESCE(NULLIF(excluded.aad_group_id, ''), teams_conversations.aad_group_id),
			tenant_id    = COALESCE(NULLIF(excluded.tenant_id, ''), teams_conversations.tenant_id),
			updated_at   = excluded.updated_at`,
		c.Connection, c.Channel, c.ServiceURL, c.TeamID, c.AADGroupID, c.TenantID, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("remember teams conversation %s of %s: %w", c.Channel, c.Connection, err)
	}
	return nil
}

// GetTeamsConversation implements store.TeamsConversations.
func (s *adapter) GetTeamsConversation(ctx context.Context, connection, channel string) (store.TeamsConversation, error) {
	c := store.TeamsConversation{Connection: connection, Channel: channel}
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT service_url, team_id, aad_group_id, tenant_id, updated_at FROM teams_conversations WHERE connection = $1 AND channel = $2`,
		connection, channel).Scan(&c.ServiceURL, &c.TeamID, &c.AADGroupID, &c.TenantID, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TeamsConversation{}, fmt.Errorf("get teams conversation %s of %s: %w", channel, connection, store.ErrNotFound)
	} else if err != nil {
		return store.TeamsConversation{}, fmt.Errorf("get teams conversation %s of %s: %w", channel, connection, err)
	}
	c.UpdatedAt = utc(at)
	return c, nil
}

// DeleteTeamsConversations implements store.TeamsConversations. It is a write transaction, so it carries the epoch check of every write.
func (s *adapter) DeleteTeamsConversations(ctx context.Context, connection string) error {
	if _, err := s.exec(ctx, `DELETE FROM teams_conversations WHERE connection = $1`, connection); err != nil {
		return fmt.Errorf("delete the teams conversations of %s: %w", connection, err)
	}
	return nil
}
