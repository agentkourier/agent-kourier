package store

import (
	"context"
	"fmt"
	"slices"
)

// SaveTeamsPollPosition implements TeamsPollPositions. No error names the deltaLink, which carries Graph's state token.
func (g *Guarded) SaveTeamsPollPosition(ctx context.Context, p TeamsPollPosition) error {
	return g.run(ctx, "agentkourier.store.SaveTeamsPollPosition", func(ctx context.Context) error {
		if p.Connection == "" || p.Channel == "" || p.DeltaLink == "" {
			return fmt.Errorf("save teams poll position: a connection, a channel and a deltaLink are required: %w", ErrInvalid)
		}
		if !validTime(p.PolledAt) {
			return fmt.Errorf("save teams poll position at %v: the time must be after the Unix epoch: %w", p.PolledAt, ErrInvalid)
		}
		return g.a.SaveTeamsPollPosition(ctx, p)
	})
}

// GetTeamsPollPosition implements TeamsPollPositions.
func (g *Guarded) GetTeamsPollPosition(ctx context.Context, connection, channel string) (p TeamsPollPosition, err error) {
	err = g.run(ctx, "agentkourier.store.GetTeamsPollPosition", func(ctx context.Context) error {
		if connection == "" || channel == "" {
			return fmt.Errorf("get teams poll position: a connection and a channel are required: %w", ErrInvalid)
		}
		var err error
		p, err = g.a.GetTeamsPollPosition(ctx, connection, channel)
		return err
	})
	return p, err
}

// DeleteTeamsPollPosition implements TeamsPollPositions.
func (g *Guarded) DeleteTeamsPollPosition(ctx context.Context, connection, channel string) error {
	return g.run(ctx, "agentkourier.store.DeleteTeamsPollPosition", func(ctx context.Context) error {
		if connection == "" || channel == "" {
			return fmt.Errorf("delete teams poll position: a connection and a channel are required: %w", ErrInvalid)
		}
		return g.a.DeleteTeamsPollPosition(ctx, connection, channel)
	})
}

// DeleteTeamsPollPositions implements TeamsPollPositions.
func (g *Guarded) DeleteTeamsPollPositions(ctx context.Context, connection string) error {
	return g.run(ctx, "agentkourier.store.DeleteTeamsPollPositions", func(ctx context.Context) error {
		if connection == "" {
			return fmt.Errorf("delete teams poll positions: a connection is required: %w", ErrInvalid)
		}
		return g.a.DeleteTeamsPollPositions(ctx, connection)
	})
}

// DeleteTeamsPollPositionsExcept implements TeamsPollPositions.
func (g *Guarded) DeleteTeamsPollPositionsExcept(ctx context.Context, connection string, keep []string) error {
	return g.run(ctx, "agentkourier.store.DeleteTeamsPollPositionsExcept", func(ctx context.Context) error {
		if connection == "" || slices.Contains(keep, "") {
			return fmt.Errorf("delete teams poll positions except: a connection, and no empty channel to keep, are required: %w", ErrInvalid)
		}
		return g.a.DeleteTeamsPollPositionsExcept(ctx, connection, keep)
	})
}
