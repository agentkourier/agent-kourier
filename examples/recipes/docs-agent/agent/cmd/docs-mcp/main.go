// Command docs-mcp serves a directory of Markdown files as an MCP server with two read-only tools, search_docs and
// read_page. By default it speaks MCP over stdin and stdout; with -http it serves streamable HTTP at every path of
// the address.
//
//	docs-mcp -corpus ./docs -site-url https://agentkourier.dev
//	docs-mcp -corpus ./docs -http 127.0.0.1:8090
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
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsmcp"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/httpserve"
)

// settings are what the command needs to start.
type settings struct {
	corpus  string
	siteURL string
	addr    string
	exclude string // comma-separated paths inside the corpus to skip
}

// parse reads the flags in args; each defaults to an environment variable: CORPUS_DIR, SITE_URL, CORPUS_EXCLUDE and MCP_ADDR.
// Help and errors of the flag package go to usage.
func parse(args []string, getenv func(string) string, usage io.Writer) (settings, error) {
	var s settings
	fs := flag.NewFlagSet("docs-mcp", flag.ContinueOnError)
	fs.SetOutput(usage)
	fs.StringVar(&s.corpus, "corpus", getenv("CORPUS_DIR"), "directory of Markdown files to serve (CORPUS_DIR)")
	fs.StringVar(&s.siteURL, "site-url", getenv("SITE_URL"), "address of the published documentation, to give page URLs (SITE_URL)")
	fs.StringVar(&s.exclude, "exclude", getenv("CORPUS_EXCLUDE"), "comma-separated paths inside the corpus to skip, such as brand,spikes (CORPUS_EXCLUDE)")
	fs.StringVar(&s.addr, "http", getenv("MCP_ADDR"), "serve streamable HTTP on this address instead of stdio (MCP_ADDR)")
	if err := fs.Parse(args); err != nil {
		return settings{}, err
	}
	if fs.NArg() > 0 {
		return settings{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if s.corpus == "" {
		return settings{}, errors.New("a corpus directory is required: -corpus or CORPUS_DIR")
	}
	return s, nil
}

func main() { os.Exit(realMain()) }

// realMain is main, with an exit status, so that its deferred calls run.
func realMain() int {
	s, err := parse(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "docs-mcp:", err)
		return 2
	}
	// Logs go to stderr: on stdio, stdout belongs to the protocol.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, s); err != nil {
		log.Error("docs-mcp stopped", "error", err)
		return 1
	}
	return 0
}

// serveStdio serves srv over t until the client goes away or ctx ends. The SDK returns the context's error when ctx
// ends, which is how a signal asks the command to stop, so that error is a clean stop and not a failure.
func serveStdio(ctx context.Context, srv *mcp.Server, t mcp.Transport) error {
	err := srv.Run(ctx, t)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func run(ctx context.Context, log *slog.Logger, s settings) error {
	ix, err := corpus.Load(ctx, s.corpus, corpus.Options{Exclude: corpus.SplitList(s.exclude)})
	if err != nil {
		return err
	}
	srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: s.siteURL})
	if err != nil {
		return err
	}
	log.Info("corpus loaded", "pages", len(ix.Paths()))
	if s.addr == "" {
		return serveStdio(ctx, srv, &mcp.StdioTransport{})
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("serving MCP over streamable HTTP", "addr", ln.Addr().String())
	return httpserve.Run(ctx, ln, docsmcp.Handler(srv), 5*time.Second)
}
