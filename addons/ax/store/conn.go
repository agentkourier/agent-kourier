package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// errNoRows is what a querier's single-row Scan returns when there is no row, on either backend.
var errNoRows = sql.ErrNoRows

// querier runs SQL written with ? placeholders.
type querier interface {
	// exec runs a statement and returns the number of rows it changed.
	exec(ctx context.Context, query string, args ...any) (int64, error)
	// queryRow runs a query for one row; its Scan returns errNoRows when there is none.
	queryRow(ctx context.Context, query string, args ...any) scanner
	// query runs a query and calls each for every row, which it closes after.
	query(ctx context.Context, query string, args []any, each func(scan func(dest ...any) error) error) error
}

type scanner interface{ Scan(dest ...any) error }

// conn is a backend's connections.
type conn interface {
	// reader runs reads outside a transaction.
	reader() querier
	// inTx runs fn in one transaction, which commits when fn returns nil and rolls back otherwise.
	inTx(ctx context.Context, fn func(q querier) error) error
	// lockMigrations runs first in each migration's transaction, so that replicas that start together apply one
	// migration at a time.
	lockMigrations(ctx context.Context, q querier) error
	// over returns a querier over a database/sql connection or transaction of this backend: one of gorm's, for the
	// fence of an ADK write.
	over(db sqlDB) querier
	close() error
}

// sqlDB is what database/sql's DB, Conn and Tx have in common, and what gorm's connection pool has.
type sqlDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqlQuerier is a querier over database/sql: SQLite's, and gorm's on Postgres, which numbers the placeholders.
type sqlQuerier struct {
	db       sqlDB
	numbered bool
}

func (q sqlQuerier) sql(query string) string {
	if q.numbered {
		return numbered(query)
	}
	return query
}

func (q sqlQuerier) exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := q.db.ExecContext(ctx, q.sql(query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (q sqlQuerier) queryRow(ctx context.Context, query string, args ...any) scanner {
	return q.db.QueryRowContext(ctx, q.sql(query), args...)
}

func (q sqlQuerier) query(ctx context.Context, query string, args []any, each func(scan func(dest ...any) error) error) error {
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

// pgQuerier is a querier over pgx (Postgres): it numbers the ? placeholders.
type pgQuerier struct {
	db interface {
		Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

func (q pgQuerier) exec(ctx context.Context, query string, args ...any) (int64, error) {
	tag, err := q.db.Exec(ctx, numbered(query), args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (q pgQuerier) queryRow(ctx context.Context, query string, args ...any) scanner {
	return pgRow{q.db.QueryRow(ctx, numbered(query), args...)}
}

func (q pgQuerier) query(ctx context.Context, query string, args []any, each func(scan func(dest ...any) error) error) error {
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
		return errNoRows
	} else if err != nil {
		return err
	}
	return nil
}

// numbered rewrites the ? placeholders of query as $1, $2, ... The store's SQL holds no ? in a literal.
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

// encodePageToken is the cursor after a task: its update time and ID.
func encodePageToken(at int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + "_" + id))
}

func decodePageToken(token string) (at int64, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	n, id, ok := strings.Cut(string(raw), "_")
	if !ok {
		return 0, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	at, err = strconv.ParseInt(n, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	return at, id, nil
}
