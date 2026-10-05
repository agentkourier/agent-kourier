// Command courier is the agent-courier broker. See spec.md for the design.
//
// It is configured by environment variables, which the Deployment sets, and takes one flag, --version, and one
// subcommand, dry-run (dryrun.go), which replays a channel's history through a Binding's chat triggers and
// reads Slack with a bot token from a file or SLACK_BOT_TOKEN:
//
//	COURIER_CONFIG_DIR     the resource files (default /etc/courier)
//	COURIER_DB_PATH        the SQLite file, on the volume (default /data/courier.db)
//	COURIER_STORE          sqlite (the default, one replica) or postgres
//	COURIER_POSTGRES_DSN_SECRET   with postgres, where the DSN is: namespace/name/key of a Secret, read through the
//	                       Secret caches (its namespace needs the list and watch Role the config's do)
//	COURIER_POSTGRES_TIMEOUTS     with postgres, courier (default: it sets the idle-in-transaction and statement
//	                       timeouts) or role (the database role sets them; for a pooler that refuses startup parameters)
//	COURIER_LEASE_NAMESPACE, COURIER_LEASE_NAME, COURIER_POD_NAME   with postgres, required: where the leader-election Lease
//	                       is (courier's own namespace, one Lease for the Deployment) and the pod's name, which names the
//	                       replica as the holder. The election runs with postgres and never with sqlite, whose replica works at once
//	COURIER_LEASE_DURATION, COURIER_LEASE_RENEW_DEADLINE, COURIER_LEASE_RETRY_PERIOD   with postgres, optional: the election's
//	                       timings (defaults 15s, 10s and 2s). A standby takes over about a lease duration after the leader's
//	                       last renewal; the leader gives up renewing after the renew deadline
//	COURIER_LISTEN         the address of /healthz, /readyz and /metrics (default :8080)
//	COURIER_SYNC_TIMEOUT   how long to wait for the Secret caches (default 30s)
//	COURIER_KLOG_V         client-go's verbosity, held below 8 (default 0)
//	COURIER_LOG_LEVEL      debug, info, warn or error (default info)
//
// It exits 0 after a signal, 1 when Slack rejects the tokens, 2 for a wrong config or wiring (or an
// unreadable variable), 3 when RBAC denies it the Secrets, 4 for any other failure, and 5 when it was the leader and
// lost the lease or met a stale epoch (spec D37): it ends at once, and Kubernetes restarts it as a standby.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/internal/broker"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

// The exit codes.
const (
	exitOK        = 0
	exitSlackAuth = 1 // Slack rejected the tokens: a restart would be rejected again
	exitUsage     = 2 // the config, the wiring or an environment variable is wrong
	exitRBAC      = 3 // the API server denied access to Secrets
	exitFailure   = 4 // anything else: a dependency failed
)

func main() {
	os.Exit(run(os.Args[1:], version, os.Stdout, os.Stderr, os.Getenv))
}

// exitCode maps what broker.Run returned to the exit code.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, broker.ErrSlackAuth):
		return exitSlackAuth
	case errors.Is(err, k8ssecrets.ErrAccessDenied):
		return exitRBAC
	case errors.Is(err, broker.ErrWiring):
		return exitUsage
	}
	return exitFailure
}

// envConfig is what the environment says.
type envConfig struct {
	configDir, dbPath, listen string
	store                     broker.StoreConfig
	election                  broker.ElectionConfig // with postgres only; its Identity is the pod's name, made unique by run
	syncTimeout               time.Duration
	klogV                     int
	logLevel                  slog.Level
	telemetry                 telemetry.Config
}

// loadEnv reads the environment through getenv. A value it cannot read is an error wrapping
// broker.ErrWiring that names the variable; nothing is guessed.
func loadEnv(getenv func(string) string) (envConfig, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := envConfig{configDir: get("COURIER_CONFIG_DIR", "/etc/courier"), dbPath: get("COURIER_DB_PATH", "/data/courier.db"), listen: get("COURIER_LISTEN", ":8080")}
	bad := func(key string, want string, err error) error {
		return fmt.Errorf("%w: %s is %q, want %s: %w", broker.ErrWiring, key, getenv(key), want, err)
	}
	var err error
	if c.syncTimeout, err = time.ParseDuration(get("COURIER_SYNC_TIMEOUT", "30s")); err != nil || c.syncTimeout <= 0 {
		return c, bad("COURIER_SYNC_TIMEOUT", "a positive duration such as 30s", errors.Join(err, errors.New("not positive")))
	}
	if c.klogV, err = strconv.Atoi(get("COURIER_KLOG_V", "0")); err != nil {
		return c, bad("COURIER_KLOG_V", "a whole number", err)
	}
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	level, ok := levels[strings.ToLower(get("COURIER_LOG_LEVEL", "info"))]
	if !ok {
		return c, bad("COURIER_LOG_LEVEL", "debug, info, warn or error", errors.New("unknown level"))
	}
	c.logLevel = level
	if c.store, err = loadStoreEnv(getenv); err != nil {
		return c, err
	}
	if c.election, err = loadElectionEnv(getenv, c.store.Kind); err != nil {
		return c, err
	}
	c.telemetry, err = telemetry.LoadConfig(getenv)
	if err != nil {
		return c, fmt.Errorf("%w: %w", broker.ErrWiring, err)
	}
	return c, nil
}

// loadStoreEnv reads the store's variables. The DSN is a secret and its variable holds only where it is, but a person may put the DSN
// there, so no error here echoes a value.
func loadStoreEnv(getenv func(string) string) (broker.StoreConfig, error) {
	bad := func(key, want string) error {
		return fmt.Errorf("%w: %s is not %s (its value is not shown)", broker.ErrWiring, key, want)
	}
	kind := strings.ToLower(getenv("COURIER_STORE"))
	if kind == "" {
		kind = string(broker.StoreSQLite)
	}
	c := broker.StoreConfig{}
	switch broker.StoreKind(kind) {
	case broker.StoreSQLite:
		c.Kind = broker.StoreSQLite
	case broker.StorePostgres:
		c.Kind = broker.StorePostgres
	default:
		return c, bad("COURIER_STORE", "sqlite or postgres")
	}
	ref, timeouts := getenv("COURIER_POSTGRES_DSN_SECRET"), getenv("COURIER_POSTGRES_TIMEOUTS")
	if c.Kind == broker.StoreSQLite {
		// Ignoring either would run on SQLite what the person meant for Postgres.
		switch {
		case ref != "":
			return c, bad("COURIER_POSTGRES_DSN_SECRET", "unset with COURIER_STORE=sqlite")
		case timeouts != "":
			return c, bad("COURIER_POSTGRES_TIMEOUTS", "unset with COURIER_STORE=sqlite")
		}
		return c, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 3 || slices.Contains(parts, "") {
		return c, bad("COURIER_POSTGRES_DSN_SECRET", "namespace/name/key, the Secret that holds the DSN")
	}
	c.PostgresDSN = broker.SecretKey{Namespace: parts[0], Name: parts[1], Key: parts[2]}
	switch timeouts {
	case "", "courier":
	case "role":
		c.PostgresRoleTimeouts = true
	default:
		return c, bad("COURIER_POSTGRES_TIMEOUTS", "courier or role")
	}
	return c, nil
}

// loadElectionEnv reads the election's variables. With postgres the Lease and the pod are required, and the timings are
// optional (zero means the default); with sqlite there is no election, and a variable that asks for one is refused,
// since ignoring it would run one replica where the person meant two.
func loadElectionEnv(getenv func(string) string, kind broker.StoreKind) (broker.ElectionConfig, error) {
	timings := map[string]*time.Duration{}
	var c broker.ElectionConfig
	timings["COURIER_LEASE_DURATION"], timings["COURIER_LEASE_RENEW_DEADLINE"], timings["COURIER_LEASE_RETRY_PERIOD"] = &c.LeaseDuration, &c.RenewDeadline, &c.RetryPeriod
	if kind != broker.StorePostgres {
		for _, key := range []string{"COURIER_LEASE_NAMESPACE", "COURIER_LEASE_NAME", "COURIER_LEASE_DURATION", "COURIER_LEASE_RENEW_DEADLINE", "COURIER_LEASE_RETRY_PERIOD"} {
			if getenv(key) != "" {
				return broker.ElectionConfig{}, fmt.Errorf("%w: %s is set with COURIER_STORE=sqlite, which has no leader election: unset it", broker.ErrWiring, key)
			}
		}
		return c, nil
	}
	c.Namespace, c.Name, c.Identity = getenv("COURIER_LEASE_NAMESPACE"), getenv("COURIER_LEASE_NAME"), getenv("COURIER_POD_NAME")
	for _, req := range []struct{ key, value string }{{"COURIER_LEASE_NAMESPACE", c.Namespace}, {"COURIER_LEASE_NAME", c.Name}, {"COURIER_POD_NAME", c.Identity}} {
		if req.value == "" {
			return c, fmt.Errorf("%w: %s is required with COURIER_STORE=postgres: more than one replica may run, and the Lease says which leads", broker.ErrWiring, req.key)
		}
	}
	for _, key := range []string{"COURIER_LEASE_DURATION", "COURIER_LEASE_RENEW_DEADLINE", "COURIER_LEASE_RETRY_PERIOD"} {
		v := getenv(key)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("%w: %s is %q, want a positive duration such as 15s", broker.ErrWiring, key, v)
		}
		*timings[key] = d
	}
	return c, nil
}

// uniqueIdentity makes the pod's name the name of this process: a container that restarts in the same pod is a new
// process, and must not take the Lease its predecessor held for its own (broker.ElectionConfig.Identity).
func uniqueIdentity(pod string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("make the replica's identity: %w", err)
	}
	return pod + "_" + hex.EncodeToString(b), nil
}

// run executes the CLI and returns the process exit code.
func run(args []string, ver string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) > 0 && args[0] == "dry-run" {
		return dryRunCommand(args[1:], stdout, stderr, getenv)
	}
	fs := flag.NewFlagSet("courier", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Flag parsing stops at the first argument that is no flag, so a mistyped subcommand (dryrun, dry_run) would
	// fall through and start the broker: in a pod, a second one. The broker takes no arguments. The argument is not
	// echoed: a person may have typed a token in its place.
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "courier: unknown command; the broker takes no arguments (did you mean dry-run?)")
		return exitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, ver)
		return exitOK
	}
	env, err := loadEnv(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	log := slog.New(telemetry.NewJSONHandler(stderr, env.logLevel))
	// A component that falls back to slog.Default() (the outbox, the sweeps, net/http's error log) goes through the
	// same handler. The one process-wide setting; it is made here and nowhere else.
	slog.SetDefault(log)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", env.listen)
	if err != nil {
		return fail(stderr, fmt.Errorf("listen on %s: %w", env.listen, err))
	}
	defer func() { _ = ln.Close() }()

	if env.store.Kind == broker.StorePostgres {
		if env.election.Identity, err = uniqueIdentity(env.election.Identity); err != nil {
			return fail(stderr, err)
		}
	}
	cfg := broker.RunConfig{
		ConfigDir: env.configDir, DBPath: env.dbPath, Store: env.store, Election: env.election, Listener: ln, SyncTimeout: env.syncTimeout, KlogVerbosity: env.klogV, Logger: log, Telemetry: env.telemetry, ServiceVersion: ver,
	}
	applyTestHooks(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return fail(stderr, broker.Run(ctx, cfg))
}

// fail reports err on stderr and returns its exit code.
func fail(stderr io.Writer, err error) int {
	code := exitCode(err)
	if err != nil {
		// Run's error may quote an agent, Slack or a dependency: redacted and capped, as in a log line.
		_, _ = fmt.Fprintln(stderr, "courier:", telemetry.LogText(err))
	}
	return code
}
