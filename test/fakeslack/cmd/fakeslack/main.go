// Command fakeslack serves the fake Slack of package fakeslack (Web API, Socket Mode, and the
// /_control/ surface) on one port, as the fast E2E tier's container. The control surface has
// no authentication: run it only behind a ClusterIP Service, and never publish its port. The
// server stops on SIGINT or SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
	"github.com/agentkourier/agent-kourier/test/httpserve"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeslack:", err)
		os.Exit(1)
	}
}

// run parses args and serves until ctx ends. The default address is loopback; the container
// passes -addr 0.0.0.0:8080.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fakeslack", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	slack := fakeslack.NewHandler()
	defer slack.Close()
	return httpserve.Run(ctx, "fakeslack", *addr, slack, out)
}
