// Package httpserve runs one of the test doubles (fake Slack, sink) as a container's main
// process: listen, announce, serve until told to stop. It is for the commands under test/.
package httpserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// shutdownGrace bounds how long Run waits for in-flight requests after its context ends.
const shutdownGrace = 5 * time.Second

// Run serves h on addr until ctx ends, then shuts down and returns nil. Once the port is
// bound it writes "<name>: listening on <address>" to out, so a script can wait for that line.
// A port that cannot be bound, or a server that fails, is an error.
func Run(ctx context.Context, name, addr string, h http.Handler, out io.Writer) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	if _, err := fmt.Fprintf(out, "%s: listening on %s\n", name, ln.Addr()); err != nil {
		_ = srv.Close()
		return fmt.Errorf("announce: %w", err)
	}
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	// The shutdown must outlive the context that starts it.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // a request that will not finish (a stream, a socket) does not hold the process
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
