// Command agent serves an ADK agent on Amazon Bedrock or the Anthropic API as an A2A agent.
//
// It is configured through its environment (package config holds every variable and its check; package telemetry the
// OTEL_* ones). On Bedrock it reads AWS credentials through the AWS SDK's default chain (AWS_BEARER_TOKEN_BEDROCK, a
// role, a profile); on the Anthropic API, the Anthropic SDK reads its own (ANTHROPIC_API_KEY first) and
// ANTHROPIC_BASE_URL. A setting it cannot use stops it at start, with an error that names the variable. It takes one flag,
// --version.
//
// Requests to the A2A endpoint and the card must pass the caller check: a token from AUTH_TOKEN_FILE, or, inside Google
// AX, an OIDC ID token for Agent Kourier's service identity. /healthz, /readyz and /metrics are open. With Postgres its
// replicas elect a leader (package serve); a replica that loses the lease exits with cluster.ExitLeadershipLost (5).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/anthropic"
	"github.com/agentkourier/agent-kourier/addons/ax/bedrock"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/serve"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

// The exit codes. A replica that loses the leadership exits with cluster.ExitLeadershipLost (5).
const (
	exitOK      = 0
	exitFailure = 1 // a dependency failed: the store, the AWS configuration, the listener
	exitUsage   = 2 // a flag, an argument or a setting is wrong
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run executes the command and returns its exit code.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("agent-kourier-ax", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		// The argument is not echoed: a person may have typed a token in its place.
		_, _ = fmt.Fprintln(stderr, "agent-kourier-ax: the agent takes no arguments; it is configured by its environment")
		return exitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return exitOK
	}
	cfg, err := config.Parse(getenv)
	var traces telemetry.Config
	if err == nil {
		traces, err = telemetry.LoadConfig(agent.TelemetrySpec().Name, getenv)
	}
	if err != nil {
		slog.New(telemetry.NewLogHandler(stderr, slog.LevelInfo)).Error("configuration", "error", err)
		return exitUsage
	}
	log := slog.New(telemetry.NewLogHandler(stderr, cfg.LogLevel))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := start(ctx, cfg, traces, log); err != nil {
		log.Error("agent stopped", "error", err)
		return exitFailure
	}
	return exitOK
}

// start builds the telemetry and the model, its calls observed, and serves the agent until ctx ends.
func start(ctx context.Context, cfg config.Config, traces telemetry.Config, log *slog.Logger) error {
	if traces.Endpoint != "" {
		telemetry.InstallDiagnostics(log)
	}
	provider, shutdownTraces, err := traces.Provider(ctx, version, log)
	if err != nil {
		return fmt.Errorf("traces: %w", err)
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), serve.ShutdownTimeout)
		defer cancel()
		_ = shutdownTraces(flush)
	}()
	tel, err := telemetry.New(agent.TelemetrySpec(), provider)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	llm, err := newModel(ctx, cfg, tel)
	if err != nil {
		return err
	}
	return serve.Run(ctx, cfg, llm, serve.Options{Version: version, Log: log, Telemetry: tel})
}

// newModel is the model of cfg's provider, its calls observed by tel.
func newModel(ctx context.Context, cfg config.Config, tel *telemetry.Telemetry) (model.LLM, error) {
	if cfg.ModelProvider == config.ProviderAnthropic {
		return &anthropic.Model{ID: cfg.ModelName, Client: anthropic.Observe(anthropic.NewClient(), tel)}, nil
	}
	aws, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.ModelRegion))
	if err != nil {
		// The SDK's error can quote a shared config file; it is not passed on.
		return nil, errors.New("bedrock: the AWS configuration could not be loaded")
	}
	return &bedrock.Model{ID: cfg.ModelName, Client: bedrock.Observe(bedrockruntime.NewFromConfig(aws), tel)}, nil
}
