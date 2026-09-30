package httpserve_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/test/httpserve"
)

// lineWriter sends each line written to it on a channel, so a test can wait for one
// without sharing a buffer with the goroutine that writes. Run writes one line, which the
// buffer of 1 holds.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

// start runs Run in the background and returns the address it announced and its result.
func start(ctx context.Context, t *testing.T, h http.Handler, opts ...httpserve.Option) (string, <-chan error) {
	t.Helper()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() { done <- httpserve.Run(ctx, "demo", "127.0.0.1:0", h, out, opts...) }()
	addr, ok := strings.CutPrefix(<-out, "demo: listening on ")
	if !ok {
		t.Fatal("Run did not announce \"demo: listening on <address>\"")
	}
	return addr, done
}

// get makes one request on its own connection, so none is left idle when a test ends.
func get(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func result(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish", what)
		return nil
	}
}

func TestRunServesAndStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })
	addr, done := start(ctx, t, h)
	if got, err := get(ctx, "http://"+addr+"/"); err != nil || got != "pong" {
		t.Fatalf("GET = %q, %v; want pong", got, err)
	}
	cancel()
	if err := result(t, done, "Run"); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
}

func TestRunReportsAPortThatIsTaken(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	out := make(lineWriter, 1)
	err = httpserve.Run(t.Context(), "demo", ln.Addr().String(), http.NotFoundHandler(), out)
	if err == nil {
		t.Fatal("Run on a taken port = nil, want an error")
	}
	select {
	case line := <-out:
		t.Fatalf("Run announced %q although it never bound", line)
	default:
	}
}

// A request in flight when the context ends is answered, and nothing new is accepted meanwhile.
func TestRunLetsARequestInFlightFinish(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	addr, done := start(ctx, t, h)
	type reply struct {
		body string
		err  error
	}
	replied := make(chan reply, 1)
	go func() {
		body, err := get(t.Context(), "http://"+addr+"/")
		replied <- reply{body, err}
	}()
	<-started

	cancel()
	// Shutdown closes the listener first; once a new connection is refused, it has begun and
	// is waiting for the request.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener still accepts connections after the context ended")
		}
	}
	close(release)

	select {
	case r := <-replied:
		if r.err != nil || r.body != "finished" {
			t.Fatalf("the request in flight got %q, %v; want its body", r.body, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request in flight was not answered")
	}
	if err := result(t, done, "Run"); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

// A request that outlasts the grace is cut off, and Run returns just after it.
func TestRunClosesConnectionsThatOutlastTheGrace(t *testing.T) {
	const grace = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // a hung request: it ends only when its connection is closed
	})
	addr, done := start(ctx, t, h, httpserve.WithShutdownGrace(grace))
	cut := make(chan error, 1)
	go func() {
		_, err := get(t.Context(), "http://"+addr+"/")
		cut <- err
	}()
	<-started

	cancelled := time.Now()
	cancel()
	if err := result(t, done, "Run"); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if took := time.Since(cancelled); took < grace || took > 5*time.Second {
		t.Errorf("Run returned %v after the context ended, want just after the %v grace", took, grace)
	}
	if err := result(t, cut, "the hung request"); err == nil {
		t.Error("the hung request succeeded, want its connection closed")
	}
}
