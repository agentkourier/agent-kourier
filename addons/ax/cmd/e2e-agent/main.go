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

	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/e2emodel"
	"github.com/agentkourier/agent-kourier/addons/ax/serve"
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
	cfg, err := config.Parse(func(key string) string {
		if v := os.Getenv(key); v != "" || (key != "MODEL_REGION" && key != "MODEL_NAME") {
			return v
		}
		return "unused-by-the-scenario-model"
	})
	if err != nil {
		return err
	}
	model := &e2emodel.Model{Scenario: scenario}
	return serve.Run(ctx, cfg, model, serve.Options{Version: "e2e", Log: slog.Default(),
		Routes: func(mux *http.ServeMux) { mux.Handle("/_control/release/", model) }})
}
