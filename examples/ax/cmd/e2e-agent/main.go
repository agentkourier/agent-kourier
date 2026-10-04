// Command e2e-agent serves the reference AX/ADK stack with a deterministic scenario model.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentkourier/agent-kourier/examples/ax/agent"
	"github.com/agentkourier/agent-kourier/examples/ax/e2emodel"
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
	issuer, audience, subject := os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_AUDIENCE"), os.Getenv("OIDC_SUBJECT")
	if scenario == "" || issuer == "" || audience == "" || subject == "" {
		return errors.New("scenario and OIDC settings are required")
	}
	verify, err := agent.OIDCAuthenticate(ctx, issuer, audience, subject)
	if err != nil {
		return err
	}

	model := &e2emodel.Model{Scenario: scenario}
	stateDir := os.Getenv("STATE_DIR")
	if stateDir == "" {
		stateDir = "/workspace/courier-state"
	}
	server, err := agent.New(ctx, agent.Config{StateDir: stateDir, Model: model, Authenticate: verify})
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	mux := http.NewServeMux()
	mux.Handle("/_control/release/", model)
	mux.Handle("/a2a", server.Handler)
	mux.Handle("/a2a/", server.Handler)
	metadata, _ := url.Parse("http://127.0.0.1:8081")
	proxy := httputil.NewSingleHostReverseProxy(metadata)
	mux.Handle("/metadata/", proxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/readyz", proxy)
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":80"
	}
	httpServer := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- httpServer.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}
