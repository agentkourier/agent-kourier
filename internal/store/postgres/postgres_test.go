package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/storetest"
)

// dsnEnv names the variable that holds the DSN of the database the tests run against. make
// test-postgres starts a throwaway container and sets it; without it the tests that need a database skip.
const dsnEnv = "AGENTKOURIER_POSTGRES_DSN"

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// serverDSN returns the DSN of the test server, or skips the test.
func serverDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " is not set: make test-postgres runs the tests that need a database against a throwaway container")
	}
	return dsn
}

// admin connects to the test server as the user of its DSN, for what a test does beside the store: set
// a lock, kill a backend, read a table. It is closed when the test ends.
func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), serverDSN(t))
	if err != nil {
		t.Fatalf("connect to the test server: %v", err) // the error is redacted by pgx; the DSN is never printed here
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

// freshSchema creates a schema that only this test uses and returns a DSN whose connections work in it
// and call themselves by a name of their own (application is that name). The store makes its tables in
// it, and the schema is dropped, with everything in it, when the test ends. The test server's DSN must
// be a URL.
func freshSchema(t *testing.T) (dsn, schema, application string) {
	t.Helper()
	base := serverDSN(t)
	schema = "agentkourier_test_" + randHex(t)
	application = "agent-kourier-test-" + randHex(t)
	if _, err := admin(t).Exec(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("connect to drop the schema: %v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("the test server's DSN must be a URL") // the error would quote the DSN
	}
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("application_name", application)
	u.RawQuery = q.Encode()
	return u.String(), schema, application
}

// openStandbyAt opens a store on dsn that has no epoch, a standby's, and closes it when the test ends.
func openStandbyAt(t *testing.T, dsn string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openAt opens a store on dsn and makes it the leader, since only the leader may write, and closes it when the test ends.
func openAt(t *testing.T, dsn string, opts ...Option) *Store {
	t.Helper()
	s := openStandbyAt(t, dsn, opts...)
	if _, err := s.Lead(t.Context(), "test-"+randHex(t)); err != nil {
		t.Fatalf("Lead: %v", err)
	}
	return s
}

// openFresh opens a store on a schema of its own.
func openFresh(t *testing.T, opts ...Option) (*Store, string) {
	t.Helper()
	dsn, _, _ := freshSchema(t)
	return openAt(t, dsn, opts...), dsn
}

// TestConformance holds the Postgres store to the suite that every store.Store implementation passes.
// Each test gets a schema of its own, so the tests do not see one another's rows. The tests of this
// package that follow are about Postgres itself: its migrations, locks, triggers and connections.
func TestConformance(t *testing.T) {
	serverDSN(t)
	storetest.Run(t, func(t *testing.T) storetest.Instance {
		dsn, _, _ := freshSchema(t)
		current := openAt(t, dsn)
		return storetest.Instance{
			Store: current,
			Reopen: func(t *testing.T) store.Store {
				t.Helper()
				if err := current.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				current = openAt(t, dsn)
				return current
			},
		}
	})
}
