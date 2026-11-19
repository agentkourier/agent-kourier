package store

import (
	"context"
	"fmt"
)

// RememberTeamsConversation implements TeamsConversations.
func (g *Guarded) RememberTeamsConversation(ctx context.Context, c TeamsConversation) error {
	return g.run(ctx, "agentkourier.store.RememberTeamsConversation", func(ctx context.Context) error {
		if c.Connection == "" || c.Channel == "" {
			return fmt.Errorf("remember teams conversation: a connection and a channel are required: %w", ErrInvalid)
		}
		if !validTime(c.UpdatedAt) {
			return fmt.Errorf("remember teams conversation at %v: the time must be after the Unix epoch: %w", c.UpdatedAt, ErrInvalid)
		}
		return g.a.RememberTeamsConversation(ctx, c)
	})
}

// GetTeamsConversation implements TeamsConversations.
func (g *Guarded) GetTeamsConversation(ctx context.Context, connection, channel string) (c TeamsConversation, err error) {
	err = g.run(ctx, "agentkourier.store.GetTeamsConversation", func(ctx context.Context) error {
		if connection == "" || channel == "" {
			return fmt.Errorf("get teams conversation: a connection and a channel are required: %w", ErrInvalid)
		}
		var err error
		c, err = g.a.GetTeamsConversation(ctx, connection, channel)
		return err
	})
	return c, err
}

// DeleteTeamsConversations implements TeamsConversations.
func (g *Guarded) DeleteTeamsConversations(ctx context.Context, connection string) error {
	return g.run(ctx, "agentkourier.store.DeleteTeamsConversations", func(ctx context.Context) error {
		if connection == "" {
			return fmt.Errorf("delete teams conversations: a connection is required: %w", ErrInvalid)
		}
		return g.a.DeleteTeamsConversations(ctx, connection)
	})
}
