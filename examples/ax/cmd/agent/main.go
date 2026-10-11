// Command agent serves the authenticated ADK/Bedrock A2A endpoint inside AX.
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
	"github.com/agentkourier/agent-kourier/examples/ax/bedrock"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

func main() {
	if err := run(); err != nil {
		slog.Error("AX agent stopped", "reason", "startup or server failure")
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	region, model := os.Getenv("MODEL_REGION"), os.Getenv("MODEL_NAME")
	issuer, audience, subject := os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_AUDIENCE"), os.Getenv("OIDC_SUBJECT")
	if region == "" || model == "" || issuer == "" || audience == "" || subject == "" {
		return errors.New("model region, model ID and OIDC settings are required")
	}
	if p := os.Getenv("MODEL_PROVIDER"); p != "" && p != "bedrock" {
		return errors.New("only Bedrock is supported")
	}
	verify, err := agent.OIDCAuthenticate(ctx, issuer, audience, subject)
	if err != nil {
		return err
	}

	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return errors.New("bedrock configuration failed")
	}
	stateDir := os.Getenv("STATE_DIR")
	if stateDir == "" {
		stateDir = "/workspace/agent-kourier-state"
	}
	server, err := agent.New(ctx, agent.Config{StateDir: stateDir, Model: &bedrock.Model{ID: model, Client: bedrockruntime.NewFromConfig(awsConfig)}, Authenticate: verify})
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	mux := http.NewServeMux()
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
