// Command hitlagent serves the hitlagent test agent: A2A 1.0 JSON-RPC with streaming at "/", and its card at
// "/.well-known/agent-card.json". It listens on :$PORT (default 8080); $AGENT_URL sets the card's interface URL.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentkourier/agent-kourier/test/hitlagent"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hitlagent stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	agentURL := os.Getenv("AGENT_URL")
	if agentURL == "" {
		agentURL = hitlagent.DefaultAgentURL
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           hitlagent.NewHandler(agentURL),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("hitlagent listening", "addr", srv.Addr, "agent_url", agentURL)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve %s: %w", srv.Addr, err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", srv.Addr, err)
	}
	return nil
}
