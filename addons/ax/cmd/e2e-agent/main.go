// Command e2e-agent serves the reference AX/ADK stack with a deterministic scenario model, for the kind AX tier. It
// reads the agent's settings as the agent does, except that the model is AX_SCENARIO's and needs no MODEL_REGION or
// MODEL_NAME.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/e2emodel"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("AX agent stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	scenario := os.Getenv("AX_SCENARIO")
	if scenario == "" {
		return errors.New("AX_SCENARIO is required")
	}
	scripted := &e2emodel.Model{Scenario: scenario}
	addon := agent.Addon("e2e", func(context.Context, config.Config, *telemetry.Telemetry) (model.LLM, error) { return scripted, nil })
	cfg, own, err := runtime.Parse(addon, func(key string) string {
		if v := os.Getenv(key); v != "" || (key != "MODEL_REGION" && key != "MODEL_NAME") {
			return v
		}
		return "unused-by-the-scenario-model"
	})
	if err != nil {
		return err
	}
	return runtime.Run(ctx, addon, cfg, own, runtime.Options{Log: slog.Default(),
		Routes: func(mux *http.ServeMux) { mux.Handle("/_control/release/", scripted) }})
}
