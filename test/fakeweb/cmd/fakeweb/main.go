// Command fakeweb serves the fake of package fakeweb on one port, as the fast E2E tier's container: the sender of the web
// connections' requests and the receiver of the kourier-v1 connection's replies. Its control surface has no authentication: run it
// only behind a ClusterIP Service, and never publish its port. The server stops on SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentkourier/agent-kourier/test/fakeweb"
	"github.com/agentkourier/agent-kourier/test/httpserve"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeweb:", err)
		os.Exit(1)
	}
}

// run parses args and serves until ctx ends. The default address is loopback; the container passes -addr 0.0.0.0:8080. The secret
// is a fake constant of the tier; an error never repeats it.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fakeweb", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the flag package repeats a rejected value in its message, and a value may be a secret
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	chatURL := fs.String("chat-url", "", "the broker's chat listener, scheme and host, which the sender POSTs to")
	kourier := fs.String("kourier-v1-secret", "", "the signingSecret of the kourier-v1 connection")
	if err := fs.Parse(args); err != nil {
		return errors.New("flags: want -addr, -chat-url and -kourier-v1-secret")
	}
	srv, err := fakeweb.New(fakeweb.Config{ChatURL: *chatURL, KourierV1Secret: []byte(*kourier)})
	if err != nil {
		return err
	}
	return httpserve.Run(ctx, "fakeweb", *addr, srv, out)
}
