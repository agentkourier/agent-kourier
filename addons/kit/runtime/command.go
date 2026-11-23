package runtime

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// The exit codes. A replica that loses the leadership exits with cluster.ExitLeadershipLost (5).
const (
	ExitOK      = 0
	ExitFailure = 1 // a dependency failed: the store, the backend, the listener
	ExitUsage   = 2 // a flag, an argument or a setting is wrong
)

// Main runs the add-on as its command, agent-kourier-<Name>, and exits with Command's code.
func Main[S Store](a Addon[S]) {
	os.Exit(Command(a, os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// Command runs the add-on with args, reading its settings through getenv (os.Getenv in production; an empty value is
// an unset one), and returns its exit code. It takes one flag, --version, and no arguments. A setting it cannot use
// stops it with ExitUsage and one JSON log line, "configuration", that names the variable; a failure once it runs, with
// ExitFailure and the log line "<Noun> stopped". It serves until SIGINT or SIGTERM.
func Command[S Store](a Addon[S], args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if err := a.check(); err != nil {
		slog.New(telemetry.NewLogHandler(stderr, slog.LevelInfo)).Error("add-on", "error", err)
		return ExitFailure
	}
	name := "agent-kourier-" + a.Name
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		// The argument is not echoed: a person may have typed a token in its place.
		_, _ = fmt.Fprintf(stderr, "%s: the %s takes no arguments; it is configured by its environment\n", name, a.Noun)
		return ExitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, a.Version)
		return ExitOK
	}
	cfg, own, err := Parse(a, getenv)
	var traces telemetry.Config
	if err == nil {
		traces, err = telemetry.LoadConfig(a.Name, getenv)
	}
	if err != nil {
		slog.New(telemetry.NewLogHandler(stderr, slog.LevelInfo)).Error("configuration", "error", err)
		return ExitUsage
	}
	log := slog.New(telemetry.NewLogHandler(stderr, cfg.LogLevel))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := start(ctx, a, cfg, own, traces, log); err != nil {
		log.Error(a.Noun+" stopped", "error", err)
		return ExitFailure
	}
	return ExitOK
}

// Parse reads the add-on's settings through getenv: its own (Settings) and the shared ones.
func Parse[S Store](a Addon[S], getenv func(string) string) (settings.Config, settings.Values, error) {
	return settings.Parse(getenv, settings.Spec{Name: a.Name, Noun: a.Noun, OIDC: a.OIDC != nil, Vars: a.Settings})
}

// start builds the telemetry, its traces exported as traces says, and runs the add-on until ctx ends; the spans are
// flushed, within ShutdownTimeout, when it returns.
func start[S Store](ctx context.Context, a Addon[S], cfg settings.Config, own settings.Values, traces telemetry.Config, log *slog.Logger) error {
	if traces.Endpoint != "" {
		telemetry.InstallDiagnostics(log)
	}
	provider, shutdownTraces, err := traces.Provider(ctx, a.Version, log)
	if err != nil {
		return fmt.Errorf("traces: %w", err)
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
		defer cancel()
		_ = shutdownTraces(flush)
	}()
	tel, err := telemetry.New(a.TelemetrySpec(), provider)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	return Run(ctx, a, cfg, own, Options{Log: log, Telemetry: tel})
}
