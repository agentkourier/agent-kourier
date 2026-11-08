package store

import (
	"context"
	"fmt"
	"slices"
)

// ServedConnections implements ConnectionServing.
func (g *Guarded) ServedConnections(ctx context.Context) (connections []string, err error) {
	err = g.run(ctx, "agentkourier.store.ServedConnections", func(ctx context.Context) error {
		var err error
		connections, err = g.a.ServedConnections(ctx)
		return err
	})
	return connections, err
}

// ReplaceServedConnections implements ConnectionServing. The adapter is given the set sorted and with no repeats.
func (g *Guarded) ReplaceServedConnections(ctx context.Context, connections []string) error {
	return g.run(ctx, "agentkourier.store.ReplaceServedConnections", func(ctx context.Context) error {
		if slices.Contains(connections, "") {
			return fmt.Errorf("replace served connections: a connection must not be empty: %w", ErrInvalid)
		}
		set := slices.Clone(connections)
		slices.Sort(set)
		return g.a.ReplaceServedConnections(ctx, slices.Compact(set))
	})
}
