package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.Attribution = (*adapter)(nil)

// AttributeConnection implements store.Attribution. Both updates are one transaction on the single write connection. The
// outbox is read by the first column of outbox_thread.
func (s *adapter) AttributeConnection(ctx context.Context, connection string) (store.AttributedRows, error) {
	var out store.AttributedRows
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := execRows(ctx, tx, `UPDATE outbox SET connection = ? WHERE connection = ''`, connection)
		if err != nil {
			return fmt.Errorf("outbox rows: %w", err)
		}
		out.Outbox = int(n)
		n, err = execRows(ctx, tx, `UPDATE interaction_presses SET connection = ? WHERE connection = ''`, connection)
		if err != nil {
			return fmt.Errorf("presses: %w", err)
		}
		out.Presses = int(n)
		return nil
	})
	if err != nil {
		return store.AttributedRows{}, fmt.Errorf("attribute rows to connection %q: %w", connection, err)
	}
	return out, nil
}
