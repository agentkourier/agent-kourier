// Command adapter serves a Claude Managed Agents agent as an A2A agent.
//
// It reads CMA_AGENT_ID and CMA_ENVIRONMENT_ID, and credentials as the Anthropic Go SDK does (ANTHROPIC_API_KEY, or
// the active `ant auth login` profile). LISTEN_ADDR defaults to 127.0.0.1:8090 and PUBLIC_URL, the endpoint the card
// names, to http://LISTEN_ADDR/a2a. It checks no caller identity: bind it where only the broker reaches it.
//
// SESSION_IDLE_TTL (a Go duration, default 2160h) is how long an A2A context may go without a turn before its session
// is archived, checked once a minute; 0 keeps sessions for ever. Set it to the Binding's session.threadTTL (Agent
// Kourier's default is 2160h): past that, Agent Kourier gives the thread a new context and the old session is never
// used again. A shorter value archives sessions that a thread can still continue, and its next reply starts a new
// session without the earlier history. An idle session's sandbox is released by the environment's own idle
// timeout, not by this setting.
//
// TOOL_RESULT_TIMEOUT (a Go duration, default 10m) is how long a turn waits, with no session event, for the result
// of a built-in tool call (one the agent may run, or one a person approved); 0 waits for ever. Past it the adapter
// sends the session an interrupt and fails the turn with a message that names the sandbox. A self-hosted sandbox
// that cannot start (a full node, for instance) sends no event at all, and neither does a tool that runs silently,
// so a command that runs longer than this is cut off too.
//
// At start it lists the agent's unarchived sessions and maps each A2A context back to its session (the
// a2a_context_id metadata it gave them), so a restart does not orphan them; it does not start if that list fails.
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

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/agentkourier/agent-kourier/addons/cma/adapter"
)

func main() {
	if err := run(); err != nil {
		slog.Error("adapter stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	agentID, envID := os.Getenv("CMA_AGENT_ID"), os.Getenv("CMA_ENVIRONMENT_ID")
	if agentID == "" || envID == "" {
		return errors.New("CMA_AGENT_ID and CMA_ENVIRONMENT_ID are required")
	}
	addr := getenv("LISTEN_ADDR", "127.0.0.1:8090")
	public := getenv("PUBLIC_URL", "http://"+addr+"/a2a")

	ttl, err := time.ParseDuration(getenv("SESSION_IDLE_TTL", "2160h"))
	if err != nil || ttl < 0 {
		return fmt.Errorf("SESSION_IDLE_TTL %q is not a duration of zero or more", os.Getenv("SESSION_IDLE_TTL"))
	}

	toolTimeout, err := time.ParseDuration(getenv("TOOL_RESULT_TIMEOUT", "10m"))
	if err != nil || toolTimeout < 0 {
		return fmt.Errorf("TOOL_RESULT_TIMEOUT %q is not a duration of zero or more", os.Getenv("TOOL_RESULT_TIMEOUT"))
	}

	exec := &adapter.Executor{
		Backend:           &adapter.SDK{Client: anthropic.NewClient(), AgentID: agentID, EnvironmentID: envID},
		IdleTTL:           ttl,
		ToolResultTimeout: toolTimeout,
	}
	if err := exec.Restore(ctx); err != nil {
		return err
	}
	go exec.Run(ctx)
	card := adapter.Card("claude-managed-agent", "A Claude Managed Agents agent served over A2A", public)
	srv := &http.Server{Addr: addr, Handler: adapter.Mux("/a2a", card, adapter.NewRequestHandler(exec)), ReadHeaderTimeout: 10 * time.Second}

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	slog.Info("adapter listening", "addr", addr, "card", public+"/.well-known/agent-card.json", "agent", agentID, "environment", envID, "session_idle_ttl", ttl, "tool_result_timeout", toolTimeout)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
