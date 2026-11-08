package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.ConnectionServing = (*adapter)(nil)

// ServedConnections implements store.ConnectionServing. The order is the bytes' (the C collation), as the store promises.
func (s *adapter) ServedConnections(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT connection FROM served_connections ORDER BY `+Dialect{}.Bytewise("connection"))
	if err != nil {
		return nil, fmt.Errorf("list served connections: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list served connections: %w", err)
	}
	return out, nil
}

// ReplaceServedConnections implements store.ConnectionServing. connections has no repeats (the guard sees to it). The write is
// one transaction, so it carries the epoch check of every write.
func (s *adapter) ReplaceServedConnections(ctx context.Context, connections []string) error {
	if connections == nil {
		connections = []string{} // a nil slice would bind as NULL, and "<> ALL (NULL)" deletes nothing
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM served_connections WHERE connection <> ALL ($1::text[])`, connections); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO served_connections (connection, served_since) SELECT unnest($1::text[]), $2 ON CONFLICT (connection) DO NOTHING`,
			connections, time.Now())
		return err
	})
	if err != nil {
		return fmt.Errorf("replace served connections: %w", err)
	}
	return nil
}
