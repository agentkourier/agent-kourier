package httpserve_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/httpserve"
)

func TestRunServesUntilTheContextEnds(t *testing.T) {
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- httpserve.Run(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }), time.Second)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello" {
		t.Errorf("body = %q", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
}

func TestRunReportsAServerFailure(t *testing.T) {
	ln := listen(t)
	_ = ln.Close() // Serve fails at once on a closed listener
	if err := httpserve.Run(t.Context(), ln, http.NotFoundHandler(), time.Second); err == nil {
		t.Error("Run on a closed listener returned nil")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestLoopbackServesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	url, done, err := httpserve.Loopback(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("url = %q, want a loopback address", url)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("done: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not stop")
	}
}

// TestRunClosesWhatOutlivesTheGrace pins that a request still running when the grace ends is cut off: its context is
// cancelled, and Run reports that the shutdown did not finish.
func TestRunClosesWhatOutlivesTheGrace(t *testing.T) {
	ln := listen(t)
	started, cancelled := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- httpserve.Run(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done() // a stream that never ends by itself
			close(cancelled)
		}), 100*time.Millisecond)
	}()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run = %v, want an error wrapping the grace's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the grace ended")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the handler's context was never cancelled: the connection was left open")
	}
}

// closeRecorder records whether its listener has been closed.
type closeRecorder struct {
	net.Listener
	closed atomic.Bool
}

func (l *closeRecorder) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

// TestRunHasClosedTheListenerWhenItReturns pins that Run's return means the listener is closed, even when the context
// has ended before the server got going: a caller that dials after Run returns is refused, not queued on a listener
// that nothing accepts from. The loop is for the race between Serve starting and the shutdown.
func TestRunHasClosedTheListenerWhenItReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for i := range 200 {
		ln := &closeRecorder{Listener: listen(t)}
		if err := httpserve.Run(ctx, ln, http.NotFoundHandler(), time.Second); err != nil {
			t.Fatalf("run %d: Run = %v, want nil", i, err)
		}
		if !ln.closed.Load() {
			t.Fatalf("run %d: Run returned before the listener was closed", i)
		}
	}
}

// acceptedListener reports each connection it accepts, once the server has it.
type acceptedListener struct {
	net.Listener
	accepted chan net.Conn
}

func (l *acceptedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted <- c
	}
	return c, err
}

// TestRunDoesNotWaitForAConnectionThatSentNothing pins that a connection that is open but has not sent a request does
// not hold up the shutdown. Such a connection is what a client's pool keeps when it dialled for a request that
// another connection then served; net/http's Shutdown waits up to 5 seconds for one, which is the whole grace of
// the embedded MCP server.
func TestRunDoesNotWaitForAConnectionThatSentNothing(t *testing.T) {
	ln := &acceptedListener{Listener: listen(t), accepted: make(chan net.Conn, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- httpserve.Run(ctx, ln, http.NotFoundHandler(), 2*time.Second) }()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	select {
	case <-ln.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never accepted the connection")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil: a connection with no request has nothing to drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read on the idle connection after Run = %v, want EOF: the server should have closed it", err)
	}
}
