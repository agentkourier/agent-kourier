package postgres

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.Attribution = (*adapter)(nil)

// AttributeConnection implements store.Attribution. Each batch of store.AttributeBatch rows is a write of its own, so it carries
// the epoch check of every write and stays short of the statement timeout however large the table is. The outbox is read by
// the first column of outbox_thread.
func (s *adapter) AttributeConnection(ctx context.Context, connection string) (store.AttributedRows, error) {
	var out store.AttributedRows
	var err error
	if out.Outbox, err = s.attribute(ctx, `UPDATE outbox SET connection = $1 WHERE id IN (SELECT id FROM outbox WHERE connection = '' LIMIT $2)`, connection); err != nil {
		return store.AttributedRows{}, fmt.Errorf("attribute outbox rows to connection %q: %w", connection, err)
	}
	if out.Presses, err = s.attribute(ctx, `UPDATE interaction_presses SET connection = $1 WHERE seq IN (SELECT seq FROM interaction_presses WHERE connection = '' LIMIT $2)`, connection); err != nil {
		return store.AttributedRows{}, fmt.Errorf("attribute presses to connection %q: %w", connection, err)
	}
	return out, nil
}

// attribute runs the batch statement update until it changes nothing, and returns how many rows it changed in all.
func (s *adapter) attribute(ctx context.Context, update, connection string) (int, error) {
	total := 0
	for {
		n, err := s.exec(ctx, update, connection, store.AttributeBatch)
		if err != nil {
			return total, err
		}
		total += int(n)
		if n < store.AttributeBatch {
			return total, nil
		}
	}
}
