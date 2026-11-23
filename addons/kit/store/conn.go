package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNoRows is what a Querier's single-row Scan returns when there is no row, on either backend.
var ErrNoRows = sql.ErrNoRows

// Querier runs SQL written with ? placeholders, on either backend: the Postgres one numbers them. The SQL holds no ? in
// a literal.
type Querier interface {
	// Exec runs a statement and returns the number of rows it changed.
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	// QueryRow runs a query for one row; its Scan returns ErrNoRows when there is none.
	QueryRow(ctx context.Context, query string, args ...any) Scanner
	// Query runs a query and calls each for every row, which it closes after.
	Query(ctx context.Context, query string, args []any, each func(scan func(dest ...any) error) error) error
}

// Scanner is one row of QueryRow.
type Scanner interface{ Scan(dest ...any) error }

// SQLDB is what database/sql's DB, Conn and Tx have in common, and what gorm's connection pool has: a connection or
// transaction that Fence can run the fence in.
type SQLDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// conn is a backend's connections.
type conn interface {
	// reader runs reads outside a transaction.
	reader() Querier
	// inTx runs fn in one transaction, which commits when fn returns nil and rolls back otherwise.
	inTx(ctx context.Context, fn func(q Querier) error) error
	// lockMigrations runs first in each migration's transaction, so that replicas that start together apply one
	// migration at a time.
	lockMigrations(ctx context.Context, q Querier) error
	// withMigrationLock runs fn, a migration that runs on connections of its own, while this backend's migration lock
	// is held.
	withMigrationLock(ctx context.Context, fn func() error) error
	// over returns a Querier over a database/sql connection or transaction of this backend: one of gorm's, for the
	// fence of an ADK write.
	over(db SQLDB) Querier
	// sql is the write connections as a database/sql DB.
	sql() *sql.DB
	close() error
}

// sqlQuerier is a Querier over database/sql: SQLite's, and gorm's on Postgres, which numbers the placeholders.
type sqlQuerier struct {
	db       SQLDB
	numbered bool
}

func (q sqlQuerier) sql(query string) string {
	if q.numbered {
		return numbered(query)
	}
	return query
}

func (q sqlQuerier) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := q.db.ExecContext(ctx, q.sql(query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (q sqlQuerier) QueryRow(ctx context.Context, query string, args ...any) Scanner {
	return q.db.QueryRowContext(ctx, q.sql(query), args...)
}

func (q sqlQuerier) Query(ctx context.Context, query string, args []any, each func(scan func(dest ...any) error) error) error {
	rows, err := q.db.QueryContext(ctx, q.sql(query), args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := each(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// pgQuerier is a Querier over pgx (Postgres): it numbers the ? placeholders.
type pgQuerier struct {
	db interface {
		Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

func (q pgQuerier) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	tag, err := q.db.Exec(ctx, numbered(query), args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (q pgQuerier) QueryRow(ctx context.Context, query string, args ...any) Scanner {
	return pgRow{q.db.QueryRow(ctx, numbered(query), args...)}
}

func (q pgQuerier) Query(ctx context.Context, query string, args []any, each func(scan func(dest ...any) error) error) error {
	rows, err := q.db.Query(ctx, numbered(query), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

type pgRow struct{ pgx.Row }

func (r pgRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); errors.Is(err, pgx.ErrNoRows) {
		return ErrNoRows
	} else if err != nil {
		return err
	}
	return nil
}

// numbered rewrites the ? placeholders of query as $1, $2, ... The SQL holds no ? in a literal.
func numbered(query string) string {
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
