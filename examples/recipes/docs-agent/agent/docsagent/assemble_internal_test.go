package docsagent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestLoopbackClientClosesItsConnectionsWhenTheContextEnds pins what lets the embedded MCP server stop inside its
// grace: a connection the client keeps open is one the server's Shutdown waits for.
func TestLoopbackClientClosesItsConnectionsWhenTheContextEnds(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed {
			once.Do(func() { close(closed) })
		}
	}
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := loopbackClient(ctx)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close() // the connection is now idle in the client's pool

	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the client kept its connection to the server open after the context ended")
	}
}
