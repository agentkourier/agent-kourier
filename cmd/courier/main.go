// Command courier is the agent-courier broker. See spec.md for the design.
//
// It is configured by environment variables, which the Deployment sets, and takes one flag, --version, and one
// subcommand, dry-run (dryrun.go), which replays a channel's history through a Binding's chat triggers and
// reads Slack with a bot token from a file or SLACK_BOT_TOKEN:
//
//	COURIER_CONFIG_DIR     the resource files (default /etc/courier)
//	COURIER_DB_PATH        the SQLite file, on the volume (default /data/courier.db)
//	COURIER_LISTEN         the address of /healthz, /readyz and /metrics (default :8080)
//	COURIER_SYNC_TIMEOUT   how long to wait for the Secret caches (default 30s)
//	COURIER_KLOG_V         client-go's verbosity, held below 8 (default 0)
//	COURIER_LOG_LEVEL      debug, info, warn or error (default info)
//
// It exits 0 after a signal, 1 when Slack rejects the tokens, 2 for a wrong config or wiring (or an
// unreadable variable), 3 when RBAC denies it the Secrets, and 4 for any other failure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
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
	c.telemetry, err = telemetry.LoadConfig(getenv)
	if err != nil {
		return c, fmt.Errorf("%w: %w", broker.ErrWiring, err)
	}
	return c, nil
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
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: env.logLevel}))
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", env.listen)
	if err != nil {
		return fail(stderr, fmt.Errorf("listen on %s: %w", env.listen, err))
	}
	defer func() { _ = ln.Close() }()

	cfg := broker.RunConfig{
		ConfigDir: env.configDir, DBPath: env.dbPath, Listener: ln, SyncTimeout: env.syncTimeout, KlogVerbosity: env.klogV, Logger: log, Telemetry: env.telemetry, ServiceVersion: ver,
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
		_, _ = fmt.Fprintln(stderr, "courier:", err)
	}
	return code
}
