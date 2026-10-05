// Command signaler serves the container signaler of package signaler on one port, as the fast E2E tier's HA helper. It
// has no authentication and can stop or kill any process of its node: run it only behind a ClusterIP Service, in a
// test cluster. The server stops on SIGINT or SIGTERM.
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
	"github.com/agentkourier/agent-kourier/test/signaler"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "signaler:", err)
		os.Exit(1)
	}
}

// run parses args and serves until ctx ends. The default address is loopback; the container passes -addr 0.0.0.0:8080.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("signaler", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	return httpserve.Run(ctx, "signaler", *addr, signaler.NewHandler(), out)
}
