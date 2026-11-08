package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.ConnectionServing = (*adapter)(nil)

// ServedConnections implements store.ConnectionServing.
func (s *adapter) ServedConnections(ctx context.Context) ([]string, error) {
	//nolint:gosec // G202: the order is a column name through the dialect; there is no value in the statement
	rows, err := s.read.QueryContext(ctx, `SELECT connection FROM served_connections ORDER BY `+Dialect{}.Bytewise("connection"))
	if err != nil {
		return nil, fmt.Errorf("list served connections: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan a served connection: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list served connections: %w", err)
	}
	return out, nil
}

// ReplaceServedConnections implements store.ConnectionServing. connections is sorted and has no repeats (the guard sees to it).
func (s *adapter) ReplaceServedConnections(ctx context.Context, connections []string) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		have, err := recordedServed(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range have {
			if _, found := slices.BinarySearch(connections, c); !found {
				if _, err := tx.ExecContext(ctx, `DELETE FROM served_connections WHERE connection = ?`, c); err != nil {
					return err
				}
			}
		}
		since := ts(time.Now())
		for _, c := range connections {
			if _, err := tx.ExecContext(ctx, `INSERT INTO served_connections (connection, served_since) VALUES (?, ?) ON CONFLICT (connection) DO NOTHING`, c, since); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replace served connections: %w", err)
	}
	return nil
}

// recordedServed returns the connections that tx sees as recorded.
func recordedServed(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT connection FROM served_connections`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var have []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		have = append(have, c)
	}
	return have, rows.Err()
}
