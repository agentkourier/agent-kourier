// Package postgres implements store.Store on PostgreSQL with the pgx driver, for more than one
// replica (spec D15, D37). SQLite stays the default for one.
//
// # Pool and transactions
//
// A pgx pool serves every call. Reads go straight to the pool. Every write goes through adapter.inTx,
// the only function that opens a write transaction, so that the one rule H4-5 adds (check the
// leader's epoch first, refuse a stale one) has one place to live. TestWritesGoThroughInTx holds the
// package to it.
//
// # Ordering that Postgres does not give for free
//
// A value from an identity column is taken when a row is inserted, and the row is shown to other
// sessions when its transaction commits, so a reader that walks a table by that value could see 6
// before 5 and never see 5. The reply queue (a reader walks a session's queue with NextReplyAfter) and
// the audit log (ListAudit pages by sequence) both promise that a row is shown only after every row
// with a lower sequence is. Each insert therefore runs inside a transaction that first takes a
// transaction-level advisory lock, and the lock is released only when the row is committed:
//
//   - the reply queue takes one lock per session, so sessions do not wait for one another;
//   - the audit log takes one lock for the whole log, as its sequence spans all of it.
//
// A per-session counter row, updated in the same transaction, would also serialise the enqueues and
// number a session's replies without gaps. It was not chosen: courier orders by Sequence and never
// needs it dense, the counter costs a hot row, an update's dead tuple and a second table for every
// enqueue, and the advisory lock leaves the one identity column and the one insert. The lock key is a
// hash of the session, so two sessions that collide only wait for one another; that costs nothing but
// a moment.
//
// The other operations that need one winner among concurrent callers do not rely on an isolation
// level above READ COMMITTED. A press limit and the trigger limits count rows and then insert, so
// they take one lock for all callers (RecordPress, AdmitTriggerRun): both are low-volume and brief. The
// rest are single statements that Postgres makes atomic by row, which is how ClaimEvent (FOR UPDATE SKIP
// LOCKED), the fenced Complete and Fail, the first answer to an interaction, and the compare-and-set
// of a message ref each have exactly one winner.
//
// # Cancellation and connection loss
//
// A call whose context has ended returns an error that wraps the context's, and changes nothing. A
// context that ends while a transaction is committing leaves it unknown whether it committed, as
// with any database; so does a connection that is lost then. Nothing here retries a write for that
// reason: what courier writes is idempotent or fenced (an event ID, a claim's attempt, an outbox ID), and
// its callers retry. The pool replaces a connection the server dropped, so the call after a loss works.
//
// # Versions
//
// The schema and the SQL use what PostgreSQL 11 and later have (identity columns, EXECUTE FUNCTION in
// triggers); the tests run on 17. The migrations create their tables in the first schema of the
// connection's search_path, and the role that opens the store needs the right to create there.
//
// # Times
//
// Times are timestamptz, which Postgres keeps to the microsecond and returns in UTC here. A time never
// set is NULL, and reads back as the zero time.
//
// # The DSN
//
// The DSN is a secret. Open never logs it, and no error that leaves this package carries it or the
// password in it (see redact.go).
//
// # Connection poolers
//
// The idle-in-transaction and statement timeouts are sent as startup parameters, and they are what
// ends a stalled leader's transaction and frees its locks (spec D37). PgBouncer in transaction mode
// rejects them ("unsupported startup parameter"), and listing them in ignore_startup_parameters
// silently drops that protection. Connect courier directly or through a pooler in session mode, or set
// both timeouts on the role (ALTER ROLE courier SET idle_in_transaction_session_timeout = '10s', and
// the same for statement_timeout) and pass WithIdleInTxTimeout(0) and WithStatementTimeout(0).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// defaultMaxConns is the size of the pool unless WithMaxConns says otherwise. One leader does the
// work (spec D37), so a handful of connections is plenty; the concurrent writers of the tests wait
// for one.
const defaultMaxConns = 10

// defaultConnectTimeout bounds one attempt to reach the server, unless the DSN sets connect_timeout.
const defaultConnectTimeout = 10 * time.Second

// defaultIdleInTxTimeout is how long a transaction may sit idle between statements before the server
// ends it, unless WithIdleInTxTimeout says otherwise. Every locked write spans several round trips, so
// a leader that stalls or is cut off from the server in the middle of one holds an advisory lock (the
// log's, a session's) and a share lock on the epoch row that the next leader's epoch
// update waits behind. Without a limit the server notices only when TCP gives up, about two hours
// later. Ten seconds is under the 15 s lease of D37, so the zombie's locks are gone before a standby
// may take over, and far above the milliseconds a healthy transaction idles.
const defaultIdleInTxTimeout = 10 * time.Second

// defaultStatementTimeout is how long one statement may run, lock waits included, unless
// WithStatementTimeout says otherwise. A statement of the store is an indexed lookup or a count over rows
// that are not kept long, which takes milliseconds; the limit is for the one that is stuck, most likely
// on a lock. It is above the idle timeout, so that a writer waiting for a zombie's lock outlasts the
// server ending the zombie's transaction and then goes on, and it is below the time a caller would wait
// anyway. Migrations are exempt (see applyMigration).
const defaultStatementTimeout = 30 * time.Second

// applicationName is what courier's connections call themselves in pg_stat_activity, unless the DSN
// names another.
const applicationName = "agent-courier"

// Store is a PostgreSQL-backed store.Store. It is safe for concurrent use. It is the guarded store (store.Guarded: every
// argument checked and every operation's telemetry recorded, once, for every backend, with the DSN's secrets scrubbed
// from the errors) over the Postgres adapter, which is SQL, transactions and the epoch fence and nothing else. The adapter
// is not reachable from outside the package, so that a caller cannot hold a store that skips the checks.
type Store struct {
	*store.Guarded
	a *adapter
}

var _ store.Fenced = (*Store)(nil)

// Lead takes the next epoch for holder and keeps it: from here the writes of this store are checked against it (see
// store.Fenced and the adapter's Lead). An empty holder is store.ErrInvalid.
func (s *Store) Lead(ctx context.Context, holder string) (int64, error) { return s.a.Lead(ctx, holder) }

// adapter is the Postgres backend: the SQL of every operation, the transactions it runs in and the epoch fence. It
// can rely on its arguments being legal and records no telemetry (see store.Guard).
type adapter struct {
	pool *pgxpool.Pool
	red  redactor
	// epoch is the number Lead was given, 0 before it: a store with none refuses writes.
	epoch atomic.Int64
	// deposed counts the writes that found the epoch stale (courier_store_deposed_total), and onDeposed is told of each.
	deposed   prometheus.Counter
	onDeposed func()
}

var _ store.Adapter = (*adapter)(nil)

// Option changes how Open sets up the pool.
type Option func(*options)

type options struct {
	maxConns  int32
	idleInTx  time.Duration
	statement time.Duration
	logger    *slog.Logger
	onDeposed func()
}

// WithMaxConns sets the size of the connection pool. A value below one is ignored.
func WithMaxConns(n int32) Option {
	return func(o *options) {
		if n >= 1 {
			o.maxConns = n
		}
	}
}

// WithIdleInTxTimeout sets how long a transaction may idle between statements before the server ends
// it and releases its locks (the default is 10 s, see defaultIdleInTxTimeout). Zero leaves the
// server's own setting. A DSN that sets idle_in_transaction_session_timeout itself wins.
func WithIdleInTxTimeout(d time.Duration) Option { return func(o *options) { o.idleInTx = max(d, 0) } }

// WithStatementTimeout sets how long one statement may run, lock waits included, before the server
// cancels it (the default is 30 s, see defaultStatementTimeout). Zero leaves the server's own setting.
// A DSN that sets statement_timeout itself wins. Migrations are not subject to it.
func WithStatementTimeout(d time.Duration) Option {
	return func(o *options) { o.statement = max(d, 0) }
}

// WithLogger sets where Open reports what it finds worth a warning (a schema newer than this binary).
// The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithOnDeposed sets a function that the store calls when a write finds its epoch stale, that is, when another
// replica has led since. It runs in the goroutine of the write, before the write returns store.ErrDeposed, and must
// not call the store. Courier ends its process in it (spec D37): the error alone would be handled by whichever caller
// the write happened to be made for. It is not called for a store that has no epoch yet.
func WithOnDeposed(fn func()) Option { return func(o *options) { o.onDeposed = fn } }

// Open connects to the database named by dsn, applies the migrations that are pending, and returns
// the store. The DSN is a secret: it is never logged, and every error from Open and from the store
// has it, and the password in it, replaced by "[redacted]".
//
// Open applies migrations under an advisory lock, so replicas that start together cannot both
// migrate: one applies, the others wait and find nothing to do.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	o := options{maxConns: defaultMaxConns, idleInTx: defaultIdleInTxTimeout, statement: defaultStatementTimeout, logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	red := newRedactor(dsn)
	if isURL(dsn) {
		// net/url quotes the text it chokes on, which can be a piece of the password: say what is wrong, and no more.
		if _, err := url.Parse(dsn); err != nil {
			return nil, errors.New("open postgres store: the DSN is not a valid URL: percent-encode the reserved characters of its password")
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, red.scrub(fmt.Errorf("open postgres store: parse the DSN: %w", err))
	}
	red = red.with(configSecrets(cfg)...)
	cfg.MaxConns = o.maxConns
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = defaultConnectTimeout
	}
	// Startup parameters, so that they hold on every connection without a statement of ours; a DSN that
	// sets one has the last word.
	for name, value := range map[string]string{
		"application_name":                    applicationName,
		"idle_in_transaction_session_timeout": millis(o.idleInTx),
		"statement_timeout":                   millis(o.statement),
	} {
		if _, set := cfg.ConnConfig.RuntimeParams[name]; !set && value != "" {
			cfg.ConnConfig.RuntimeParams[name] = value
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, red.scrub(fmt.Errorf("open postgres store: %w", err))
	}
	s := &adapter{pool: pool, red: red, onDeposed: o.onDeposed, deposed: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "courier_store_deposed_total", Help: "Writes the store refused because another replica has led since: its epoch was stale."})}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, red.scrub(fmt.Errorf("open postgres store: connect: %w", err))
	}
	if err := migrate(ctx, pool, migrationFiles, o.logger); err != nil {
		pool.Close()
		return nil, red.scrub(fmt.Errorf("open postgres store: %w", err))
	}
	return guard(s), nil
}

// guard returns the store that callers hold: the guarded store over a, whose errors lose the DSN's secrets (a's red).
func guard(a *adapter) *Store {
	return &Store{Guarded: store.Guard(a, store.WithScrub(a.red.scrub), store.WithNoRows(pgx.ErrNoRows)), a: a}
}

// millis formats a timeout as the milliseconds Postgres reads, or "" for none.
func millis(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// Close closes the pool, waiting for the calls in flight to give their connections back. It may be
// called more than once.
func (s *adapter) Close() error {
	s.pool.Close()
	return nil
}

// inTx runs fn in a transaction, committing if fn returns nil and rolling back on an error or a
// panic. It is the one path of every write: a statement that changes data never runs on the pool
// directly. The leader's epoch check is the top of the transaction, so that a deposed leader's write
// fails before it does anything (store.ErrDeposed, see Fencing above). It comes before any advisory lock that fn
// takes, and lock_timeout is set right after it: the new leader's epoch update waits for the writers that
// hold the epoch row, and a writer that waits for a lock while it holds that row gives up after writeLockTimeout.
//
// An error that means the data is not a legal value (text Postgres cannot store) is store.ErrInvalid.
func (s *adapter) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.checkEpoch(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, writeLockTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
		return fn(tx)
	})
	return classify(err)
}

// exec runs one statement as a write transaction and reports the rows it changed.
func (s *adapter) exec(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

// classify turns the errors that say an argument cannot be stored into store.ErrInvalid: Postgres text
// holds no NUL and only valid UTF-8, which SQLite does not insist on.
func classify(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "22021", "22P05": // character_not_in_repertoire, untranslatable_character
			return fmt.Errorf("%w: text that Postgres cannot store: %w", store.ErrInvalid, err)
		}
	}
	return err
}

// errIfUnchanged turns "no rows changed" into ErrNotFound.
func errIfUnchanged(n int64, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, store.ErrNotFound)
	}
	return nil
}

// The advisory lock classes of the first key of pg_advisory_xact_lock(int, int). The two-key form is
// a lock space of its own, apart from the one-key form, so these never meet another user of
// advisory locks that takes one bigint. The second key is a hash of the session for the reply queue
// and 0 for the rest.
const (
	lockMigrate     int32 = 0x43520001
	lockReplyQueue  int32 = 0x43520002
	lockAudit       int32 = 0x43520003
	lockPresses     int32 = 0x43520004
	lockTriggerRuns int32 = 0x43520005
)

// lock takes a transaction-level advisory lock, which waits for its holder and is released when the
// transaction ends. The key is hashed: the same text always names the same lock, and a collision of
// two keys only makes them wait for one another.
func lock(ctx context.Context, tx pgx.Tx, class int32, key string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, class, key); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	return nil
}

// sessionLockKey names a session's lock. The parts are joined with a unit separator, which a
// collision would not matter for anyway.
func sessionLockKey(k store.ConversationKey) string {
	return k.Connection + "\x1f" + k.Channel + "\x1f" + k.Thread
}

// utc returns a time read from the database in UTC, the zone the store promises.
func utc(t time.Time) time.Time { return t.UTC() }

// nullable is the value to store for a time that may be unset: the zero time is NULL.
func nullable(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// fromNull reads a time that may be NULL: NULL is the zero time.
func fromNull(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// afterEpoch reports whether t is after the Unix epoch. SQLite stores 0 for a time never set, so the
// store refuses the zero time and anything at or before the epoch for the times that SQLite could
// not tell from unset; Postgres has NULL for that, but the stores answer the same.
func afterEpoch(t time.Time) bool { return t.After(time.Unix(0, 0)) }
