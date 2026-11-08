package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.Attribution = (*adapter)(nil)

// AttributeConnection implements store.Attribution. Both updates are one transaction, so they carry the epoch check of every
// write. The outbox is read by the first column of outbox_thread.
func (s *adapter) AttributeConnection(ctx context.Context, connection string) (store.AttributedRows, error) {
	var out store.AttributedRows
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE outbox SET connection = $1 WHERE connection = ''`, connection)
		if err != nil {
			return fmt.Errorf("outbox rows: %w", err)
		}
		out.Outbox = int(tag.RowsAffected())
		tag, err = tx.Exec(ctx, `UPDATE interaction_presses SET connection = $1 WHERE connection = ''`, connection)
		if err != nil {
			return fmt.Errorf("presses: %w", err)
		}
		out.Presses = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return store.AttributedRows{}, fmt.Errorf("attribute rows to connection %q: %w", connection, err)
	}
	return out, nil
}
