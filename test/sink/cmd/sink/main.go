// Command sink serves the webhook receiver of package sink on one port, as the fast E2E tier's
// container. It has no authentication: run it only behind a ClusterIP Service. The server stops
// on SIGINT or SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentkourier/agent-kourier/test/httpserve"
	"github.com/agentkourier/agent-kourier/test/sink"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sink:", err)
		os.Exit(1)
	}
}

// run parses args and serves until ctx ends. The default address is loopback; the container
// passes -addr 0.0.0.0:8080.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sink", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	return httpserve.Run(ctx, "sink", *addr, sink.NewHandler(), out)
}
