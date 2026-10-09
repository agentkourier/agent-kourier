package broker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/postgres"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// StoreKind says which store Agent Kourier keeps its state in (spec D15).
type StoreKind string

// The stores. The zero StoreKind is SQLite, the default.
const (
	// StoreSQLite is one file on a volume, for one replica (spec R7).
	StoreSQLite StoreKind = "sqlite"
	// StorePostgres is a PostgreSQL database, which more than one replica needs, with leader election and epoch
	// fencing (spec D37).
	StorePostgres StoreKind = "postgres"
)

// SecretKey names one key of a Kubernetes Secret.
type SecretKey struct{ Namespace, Name, Key string }

// IsZero reports whether k names nothing.
func (k SecretKey) IsZero() bool { return k == SecretKey{} }

func (k SecretKey) complete() bool { return k.Namespace != "" && k.Name != "" && k.Key != "" }

// String is the reference as it is written in the environment, namespace/name/key. It names a Secret; it holds
// no value.
func (k SecretKey) String() string { return k.Namespace + "/" + k.Name + "/" + k.Key }

// StoreConfig chooses the store. Agent Kourier cannot see how many replicas run, so the one rule about them, that SQLite
// is for one, is the chart's to enforce (charts/agent-kourier refuses replicaCount above 1 with SQLite).
type StoreConfig struct {
	// Kind is StoreSQLite (also what the empty Kind means) or StorePostgres.
	Kind StoreKind
	// PostgresDSN is the Secret key that holds the Postgres DSN: required with StorePostgres, and refused with
	// SQLite. The DSN is read through the same Secret caches as the config's Secrets, so its namespace is watched.
	PostgresDSN SecretKey
	// PostgresRoleTimeouts says that the database role sets the idle-in-transaction and statement timeouts, so Agent Kourier
	// sends none: for a connection pooler in transaction mode, which refuses them as startup parameters.
	PostgresRoleTimeouts bool
}

// kind returns the store chosen, with the default applied.
func (c StoreConfig) kind() StoreKind {
	if c.Kind == "" {
		return StoreSQLite
	}
	return c.Kind
}

// validate says whether c names a store that Run can open: a wiring error otherwise. The message names the
// setting and never a value.
func (c StoreConfig) validate() error {
	switch c.kind() {
	case StoreSQLite:
		if !c.PostgresDSN.IsZero() || c.PostgresRoleTimeouts {
			return fmt.Errorf("%w: the store is sqlite, but a Postgres setting is set", ErrWiring)
		}
	case StorePostgres:
		if !c.PostgresDSN.complete() {
			return fmt.Errorf("%w: the store is postgres, which needs the namespace, name and key of the Secret that holds its DSN", ErrWiring)
		}
	default:
		return fmt.Errorf("%w: unknown store %q, want sqlite or postgres", ErrWiring, c.Kind)
	}
	return nil
}

// secretValues reads one value of a Kubernetes Secret: *k8ssecrets.Resolver.
type secretValues interface {
	Value(ctx context.Context, namespace, name, key string) (secret.Value, error)
}

// openedStore is what Run needs of a store beyond store.Store: both stores give it.
type openedStore interface {
	store.Store
	RegisterMetrics(prometheus.Registerer) error
}

// openStore opens the store that c chooses: the SQLite file at dbPath, or the Postgres database whose DSN is in the
// Secret that c names, read through secrets. extra are more options for the Postgres store (SQLite takes none): Run
// passes the one that ends the process when a write finds that another replica has led since (spec D37). An error wraps ErrWiring when the config is at fault, which includes a
// Secret that cannot be read (it is missing, has no such key, or is empty), and none carries the DSN or the password
// in it.
func openStore(ctx context.Context, c StoreConfig, dbPath string, secrets secretValues, log *slog.Logger, extra ...postgres.Option) (openedStore, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.kind() == StoreSQLite {
		st, err := sqlite.Open(ctx, dbPath)
		if err != nil {
			return nil, fmt.Errorf("open the sqlite store: %w", err)
		}
		return st, nil
	}
	dsn, err := secrets.Value(ctx, c.PostgresDSN.Namespace, c.PostgresDSN.Name, c.PostgresDSN.Key)
	if err == nil {
		// A Secret made from a file ends in a newline, which no DSN does.
		if dsn = secret.New(strings.TrimSpace(dsn.Reveal())); dsn.IsEmpty() {
			err = fmt.Errorf("secret %s/%s key %q: %w", c.PostgresDSN.Namespace, c.PostgresDSN.Name, c.PostgresDSN.Key, secret.ErrEmptyValue)
		}
	}
	if err != nil {
		// The error names the Secret and the key, which the resolver's does too, and never a value.
		return nil, fmt.Errorf("%w: the Postgres DSN: %w", ErrWiring, err)
	}
	// The errors of Open and of the store carry neither the DSN nor its password (internal/store/postgres).
	st, err := postgres.Open(ctx, dsn.Reveal(), append(postgresOptions(c, log), extra...)...)
	if err != nil {
		return nil, fmt.Errorf("open the postgres store: %w", err)
	}
	return st, nil
}

// postgresOptions are the options of postgres.Open that c asks for.
func postgresOptions(c StoreConfig, log *slog.Logger) []postgres.Option {
	opts := []postgres.Option{postgres.WithLogger(log)}
	if c.PostgresRoleTimeouts {
		opts = append(opts, postgres.WithIdleInTxTimeout(0), postgres.WithStatementTimeout(0))
	}
	return opts
}
