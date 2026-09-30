package httpserve_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/httpserve"
)

// lineWriter sends each line written to it on a channel, so a test can wait for one
// without sharing a buffer with the goroutine that writes.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

func get(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestRunServesAndStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })
	done := make(chan error, 1)
	go func() { done <- httpserve.Run(ctx, "demo", "127.0.0.1:0", h, out) }()

	line := <-out
	addr, ok := strings.CutPrefix(line, "demo: listening on ")
	if !ok {
		t.Fatalf("first line = %q, want %q followed by the address", line, "demo: listening on ")
	}
	if got := get(t, "http://"+addr+"/"); got != "pong" {
		t.Fatalf("body = %q, want pong", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
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
