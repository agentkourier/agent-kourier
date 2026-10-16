// Command docs-agent serves the docs agent over A2A. It answers questions from a directory of Markdown files, which it
// serves to itself as an MCP server on a loopback port, or from another MCP server named by MCP_URL.
//
// Settings are environment variables:
//
//	MODEL_PROVIDER, MODEL_NAME, MODEL_API_KEY, MODEL_BASE_URL   the language model (see docsagent.ModelFromEnv)
//	CORPUS_DIR       the Markdown directory (default /corpus)
//	CORPUS_EXCLUDE   comma-separated paths inside it to skip
//	SITE_URL         where the pages are published, to cite them by URL; "-" for none (default https://agentkourier.dev)
//	MCP_URL          use this MCP server's tools instead of serving CORPUS_DIR
//	LISTEN_ADDR      the A2A server's address (default :8080)
//	PUBLIC_URL       the endpoint the agent card names (default http://localhost:8080/a2a)
//	DOCS_SUBJECT     what the documentation is about, named in the prompt (default Agent Kourier)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/httpserve"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/urlcheck"
)

// settings are the non-secret settings. The model's come from docsagent.ModelFromEnv.
type settings struct {
	corpus    string
	exclude   []string
	siteURL   string
	mcpURL    string
	listen    string
	publicURL string
	subject   string
}

func settingsFromEnv(getenv func(string) string) (settings, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	s := settings{
		corpus:    or("CORPUS_DIR", "/corpus"),
		exclude:   corpus.SplitList(getenv("CORPUS_EXCLUDE")),
		siteURL:   or("SITE_URL", "https://agentkourier.dev"),
		mcpURL:    getenv("MCP_URL"),
		listen:    or("LISTEN_ADDR", ":8080"),
		publicURL: or("PUBLIC_URL", "http://localhost:8080/a2a"),
		subject:   or("DOCS_SUBJECT", docsagent.DefaultSubject),
	}
	if s.siteURL == "-" {
		s.siteURL = ""
	}
	if s.mcpURL != "" {
		if _, err := urlcheck.HTTP(s.mcpURL); err != nil {
			return settings{}, fmt.Errorf("MCP_URL %w", err)
		}
	}
	if _, err := urlcheck.HTTP(s.publicURL); err != nil {
		return settings{}, fmt.Errorf("PUBLIC_URL %w", err)
	}
	return s, nil
}

func main() { os.Exit(realMain()) }

// realMain is main, with an exit status, so that its deferred calls run. A failure is logged with its reason; the
// model's key is never in a setting that is logged or in an error.
func realMain() int {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("docs-agent stopped", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, log *slog.Logger) error {
	s, err := settingsFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	llm, err := docsagent.ModelFromEnv(ctx, os.Getenv)
	if err != nil {
		return err
	}
	// The embedded MCP server has a context of its own, which outlives the signal: the A2A server drains first, with
	// its turns still calling the tools, and only then is the MCP server stopped.
	mcpCtx, stopMCP := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMCP()
	handler, mcpDone, err := docsagent.Assemble(mcpCtx, docsagent.AssembleOptions{
		Corpus: s.corpus, Exclude: s.exclude, SiteURL: s.siteURL, MCPURL: s.mcpURL, Model: llm, PublicURL: s.publicURL, Subject: s.subject,
	})
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("serving A2A", "addr", ln.Addr().String(), "model", llm.Name(), "mcp", map[bool]string{true: "external", false: "embedded"}[s.mcpURL != ""])
	return serve(ctx, ln, handler, mcpDone, stopMCP, a2aGrace)
}

// a2aGrace is how long a stop waits for the A2A server's turns in flight. The embedded MCP server then gets its own
// grace, so a stop takes at most about 15 seconds, inside Kubernetes' default 30.
const a2aGrace = 10 * time.Second

// serve runs the A2A server on ln until ctx ends, and returns nil after a clean stop. On the stop it drains the A2A
// server first, because a turn still running calls the MCP tools, and calls stopMCP only when that has finished,
// then waits for the embedded MCP server (mcpDone, nil when the MCP server is external) to stop too. An embedded
// MCP server that stops by itself while the A2A server runs is a failure, whatever it returned: the agent can no
// longer answer, so the A2A server is stopped and the error returned.
func serve(ctx context.Context, ln net.Listener, handler http.Handler, mcpDone <-chan error, stopMCP context.CancelFunc, grace time.Duration) error {
	a2aCtx, stopA2A := context.WithCancel(ctx)
	defer stopA2A()
	a2aDone := make(chan error, 1)
	go func() { a2aDone <- httpserve.Run(a2aCtx, ln, handler, grace) }()
	select {
	case err := <-a2aDone:
		stopMCP()
		var mcpErr error
		if mcpDone != nil {
			mcpErr = <-mcpDone
		}
		return errors.Join(err, mcpErr)
	case err := <-mcpDone: // nil channel, and so never ready, when the MCP server is external
		stopA2A()
		return errors.Join(errors.New("the embedded MCP server stopped"), err, <-a2aDone)
	}
}
