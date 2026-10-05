package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

// postgresDSNEnv names the variable that holds the DSN of the database these tests run against: the one the Postgres
// store's tests use. make test-postgres starts a throwaway container and sets it; without it the tests that need a database skip.
const postgresDSNEnv = "COURIER_POSTGRES_DSN"

// postgresConfig is startConfig on the Postgres store: the DSN is in a Secret of namespace courier, which no resource of the
// example config names, and DBPath names a file that must stay unmade.
func postgresConfig(t *testing.T, dsn string) RunConfig {
	t.Helper()
	c := startConfig(t)
	s := secretOf("courier", "courier-pg", "dsn")
	s.Data["dsn"] = []byte(dsn)
	if _, err := c.Client.(*fake.Clientset).CoreV1().Secrets("courier").Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.Store = StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef}
	return c
}

// freshSchema makes a schema that only this test uses on the test server, and returns a DSN whose connections work in it. The
// schema is dropped with everything in it when the test ends.
func freshSchema(t *testing.T) (dsn, schema string) {
	t.Helper()
	base := os.Getenv(postgresDSNEnv)
	if base == "" {
		t.Skip(postgresDSNEnv + " is not set: make test-postgres runs the tests that need a database against a throwaway container")
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema = "courier_broker_test_" + hex.EncodeToString(b)
	exec := func(ctx context.Context, sql string) error {
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			return err // pgx's errors do not carry the password
		}
		defer func() { _ = conn.Close(ctx) }()
		_, err = conn.Exec(ctx, sql)
		return err
	}
	if err := exec(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		if err := exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("the test server's DSN must be a URL") // the error would quote the DSN
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), schema
}

// Run opens Postgres through the same interface as SQLite, from a DSN it reads out of a Secret, and serves on it: ready, the
// store's metrics exported, the state in the database and no SQLite file made. Neither /readyz nor /metrics shows the DSN.
func TestRunServesOnPostgresWithTheDSNFromASecret(t *testing.T) {
	dsn, schema := freshSchema(t)
	c := postgresConfig(t, dsn)
	r := start(t, c)

	r.until("/readyz", 200)

	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the SQLite file %s: %v, want it unmade on the Postgres store", filepath.Base(c.DBPath), err)
	}
	for _, path := range []string{"/readyz", "/metrics"} {
		_, body := r.get(path)
		if strings.Contains(body, dsn) || strings.Contains(body, "password") {
			t.Errorf("%s shows the DSN or its password", path)
		}
	}
	if _, body := r.get("/metrics"); !strings.Contains(body, "courier_store_pool_acquired_conns") {
		t.Error("/metrics lacks the store's pool metrics: RegisterMetrics did not run on the Postgres store")
	}
	var tables int
	conn, err := pgx.Connect(t.Context(), os.Getenv(postgresDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'events'`, schema).Scan(&tables); err != nil || tables != 1 {
		t.Errorf("the events table in schema %s: %d, %v; want the broker's state in it", schema, tables, err)
	}

	r.cancel()
	if err := r.waitWithin(30 * time.Second); err != nil {
		t.Errorf("Run = %v, want a clean stop", err)
	}
}

// A DSN Secret that cannot be used ends Run with a wiring error that says which Secret and not what is in it, and before
// anything is served ready.
func TestRunRefusesAPostgresDSNSecretThatCannotBeUsed(t *testing.T) {
	for name, tc := range map[string]struct {
		dsn  string
		key  string // the key the config asks for, when it is not the one the Secret has
		want error
	}{
		"empty":   {"", "", secret.ErrEmptyValue},
		"no key":  {"postgres://x", "other", secret.ErrKeyNotFound},
		"garbage": {"postgres://courier:" + fakePassword + "@127.0.0.1:notaport/courier", "", nil},
	} {
		t.Run(name, func(t *testing.T) {
			c := postgresConfig(t, tc.dsn)
			if tc.key != "" {
				c.Store.PostgresDSN.Key = tc.key
			}
			r := start(t, c)

			err := r.wait()

			if err == nil || strings.Contains(err.Error(), fakePassword) || strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("Run = %v, want an error without the DSN", err)
			}
			if tc.want != nil && (!errors.Is(err, ErrWiring) || !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "courier/courier-pg")) {
				t.Errorf("Run = %v, want ErrWiring wrapping %v and naming the Secret", err, tc.want)
			}
			if _, statErr := os.Stat(c.DBPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a SQLite file was made on the Postgres store: %v", statErr)
			}
		})
	}
}
