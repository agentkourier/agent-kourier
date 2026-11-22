// Package pgtest gives a test a Postgres schema of its own, on the database AGENTKOURIER_POSTGRES_DSN names, as the
// broker's Postgres tests do (hack/postgres/run.sh sets it). Only tests import it.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DSNEnv names the Postgres the tests run on. Unset, they skip.
const DSNEnv = "AGENTKOURIER_POSTGRES_DSN"

// DSN returns the DSN of a fresh schema, dropped when the test ends, or skips the test when DSNEnv is unset.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("%s is unset; hack/postgres/run.sh runs a throwaway Postgres", DSNEnv)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "kit_test_" + hex.EncodeToString(b)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		pool.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		pool.Close()
	})
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return strings.TrimSpace(dsn) + " search_path=" + schema
}
