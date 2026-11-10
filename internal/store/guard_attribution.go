package store

import (
	"context"
	"fmt"
)

// AttributeConnection implements Attribution.
func (g *Guarded) AttributeConnection(ctx context.Context, connection string) (rows AttributedRows, err error) {
	err = g.run(ctx, "agentkourier.store.AttributeConnection", func(ctx context.Context) error {
		if connection == "" {
			return fmt.Errorf("attribute connection: a connection is required: %w", ErrInvalid)
		}
		var err error
		rows, err = g.a.AttributeConnection(ctx, connection)
		return err
	})
	return rows, err
}

// UnattributedRows implements Attribution.
func (g *Guarded) UnattributedRows(ctx context.Context) (rows AttributedRows, err error) {
	err = g.run(ctx, "agentkourier.store.UnattributedRows", func(ctx context.Context) error {
		var err error
		rows, err = g.a.UnattributedRows(ctx)
		return err
	})
	return rows, err
}
