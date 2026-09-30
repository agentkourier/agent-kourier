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

// shutdownGrace is how long Run waits for requests in flight after its context ends, unless
// WithShutdownGrace says otherwise.
const shutdownGrace = 5 * time.Second

// Option configures Run.
type Option func(*config)

type config struct{ grace time.Duration }

// WithShutdownGrace sets how long Run waits for requests in flight after its context ends
// before it closes their connections. The default is 5 seconds.
func WithShutdownGrace(d time.Duration) Option { return func(c *config) { c.grace = d } }

// Run serves h on addr until ctx ends, then shuts down and returns nil. Once the port is
// bound it writes "<name>: listening on <address>" to out, so a script can wait for that line.
// A port that cannot be bound, or a server that fails, is an error.
func Run(ctx context.Context, name, addr string, h http.Handler, out io.Writer, opts ...Option) error {
	cfg := config{grace: shutdownGrace}
	for _, o := range opts {
		o(&cfg)
	}
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
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // a request that will not finish (a stream, a socket) does not hold the process
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
