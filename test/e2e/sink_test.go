package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/sink"
)

// post sends a body to the sink. It is safe to call from a goroutine: it never calls t.Fatal.
func post(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body)) //nolint:noctx // a helper against a local server
	if err != nil {
		t.Error(err)
		return
	}
	_ = resp.Body.Close()
}

func TestSinkReadsAndClearsTheLog(t *testing.T) {
	srv := httptest.NewServer(sink.NewHandler())
	defer srv.Close()
	ctx, s := context.Background(), NewSink(srv.URL)
	post(t, srv.URL+"/a", "one")
	post(t, srv.URL+"/b", "two")
	if reqs, err := s.Requests(ctx); err != nil || len(reqs) != 2 || reqs[0].Path != "/a" || reqs[1].Body != "two" {
		t.Fatalf("Requests = %+v, %v", reqs, err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if reqs, err := s.Requests(ctx); err != nil || len(reqs) != 0 {
		t.Fatalf("Clear left %v, %v", reqs, err)
	}
}

func TestSinkWaitFor(t *testing.T) {
	// The late request is sent only after the sink has answered the first poll, so the wait is
	// guaranteed to miss once and then hit, with no sleep to tune.
	var once sync.Once
	firstPoll, handler := make(chan struct{}), sink.NewHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.Method == http.MethodGet && r.URL.Path == "/requests" {
			once.Do(func() { close(firstPoll) })
		}
	}))
	defer srv.Close()
	s := NewSink(srv.URL)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		<-firstPoll
		post(t, srv.URL+"/late", "fire")
	}()
	defer func() { <-sent }()
	is := func(path string) func(sink.Request) bool { return func(r sink.Request) bool { return r.Path == path } }
	if got, err := s.WaitFor(context.Background(), 10*time.Second, is("/late")); err != nil || got.Body != "fire" {
		t.Fatalf("WaitFor = %+v, %v", got, err)
	}
	if _, err := s.WaitFor(context.Background(), 40*time.Millisecond, is("/never")); !errors.Is(err, ErrTimeout) {
		t.Fatalf("WaitFor for a request that never comes = %v, want ErrTimeout", err)
	}
}

func TestSinkReadsALogOverOneMiB(t *testing.T) {
	srv := httptest.NewServer(sink.NewHandler())
	defer srv.Close()
	for range 12 { // each is well under the sink's per-request cap; together they are 1.2 MB
		post(t, srv.URL+"/alertmanager", strings.Repeat("a", 100_000))
	}
	if reqs, err := NewSink(srv.URL).Requests(t.Context()); err != nil || len(reqs) != 12 {
		t.Fatalf("Requests = %d requests, %v; want all 12", len(reqs), err)
	}
}

func TestSinkLogOverItsCapSaysToClearIt(t *testing.T) {
	srv := httptest.NewServer(sink.NewHandler())
	defer srv.Close()
	post(t, srv.URL+"/a", strings.Repeat("a", 2000))
	s := NewSink(srv.URL)
	s.c.limit = 1000
	if _, err := s.Requests(t.Context()); err == nil || !strings.Contains(err.Error(), "Clear it") {
		t.Fatalf("Requests = %v, want an error telling the caller to Clear the log", err)
	}
}

func TestSinkErrorsSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusInternalServerError) }))
	defer srv.Close()
	ctx, s := context.Background(), NewSink(srv.URL)
	var se *StatusError
	if _, err := s.Requests(ctx); !errors.As(err, &se) || se.Status != 500 {
		t.Errorf("Requests = %v, want a 500 *StatusError", err)
	}
	if err := s.Clear(ctx); !errors.As(err, &se) {
		t.Errorf("Clear = %v, want a *StatusError", err)
	}
	// WaitFor gives the last error when it gives up, so a broken sink is not mistaken for a quiet one.
	if _, err := s.WaitFor(ctx, 40*time.Millisecond, func(sink.Request) bool { return true }); !errors.Is(err, ErrTimeout) || !errors.As(err, &se) {
		t.Errorf("WaitFor = %v, want ErrTimeout wrapping the 500", err)
	}
}
