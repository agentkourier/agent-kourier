// Package httpserve runs an HTTP server until a context ends, then shuts it down.
package httpserve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// MaxBodyBytes is the most a request body may hold: 1 MiB. A Slack message is far smaller, so the cap only stops a
// client that sends a body to fill the server's memory. A request that declares a longer body is answered with 413
// and not read; one that declares no length, or understates it, is cut at the cap and its handler's read fails.
const MaxBodyBytes = 1 << 20

// Timeouts of the server. Each bounds how long a client may hold a connection while doing nothing the server wants,
// without bounding how long the server may take to answer: a model turn streams its answer over Server-Sent Events
// for minutes, and a bound on that would cut an answer off.
const (
	// readHeaderTimeout bounds how long a client may take to send its request headers.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds reading the whole request, body included. It ends when the request has been read, so it
	// does not touch a response that is still streaming.
	readTimeout = 30 * time.Second
	// idleTimeout bounds how long a keep-alive connection may wait for its next request.
	idleTimeout = 120 * time.Second
	// There is no WriteTimeout, on purpose: it is a deadline for the whole response, so it would end every stream
	// that lasts longer than it. A stream ends when its turn does, or when the client goes away and the request's
	// context ends.
)

// newServer returns the server that Run runs: h behind the body cap, with the timeouts above.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           limitBody(h),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// limitBody enforces MaxBodyBytes on every request to h.
func limitBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		h.ServeHTTP(w, r)
	})
}

// Run serves h on ln until ctx ends, then stops accepting and waits up to grace for requests in flight. It returns
// nil after a clean shutdown, and the server's error otherwise. A request still running when the grace ends is cut
// off: its connection is closed, its context ends and the error says the shutdown did not finish. It closes ln.
// Request bodies are capped at MaxBodyBytes.
func Run(ctx context.Context, ln net.Listener, h http.Handler, grace time.Duration) error {
	srv := newServer(h)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			// The grace ran out with requests still in flight. Shutdown leaves them running, so close their
			// connections: their contexts end, and the process is not held up by a stream that never finishes.
			err = errors.Join(fmt.Errorf("shut down: %w", err), srv.Close())
		}
		// Shutdown closes only the listeners Serve has taken, and Serve may not have started yet. It returns at once
		// on a server that is shutting down, closing ln as it goes, so waiting for it is what keeps "It closes ln".
		<-done
		return err
	}
}

// Loopback serves h on a free port of the loopback interface, in the background, until ctx ends. It returns the
// server's base URL and a channel that receives Run's result once, when the server has stopped. It is how the
// docs agent reaches its own MCP server: the address never leaves the pod.
func Loopback(ctx context.Context, h http.Handler, grace time.Duration) (baseURL string, done <-chan error, err error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen on loopback: %w", err)
	}
	ch := make(chan error, 1)
	go func() { ch <- Run(ctx, ln, h, grace) }()
	return "http://" + ln.Addr().String(), ch, nil
}
