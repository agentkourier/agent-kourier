package httpserve

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestServerTimeouts pins the choice of timeouts: every read-side bound is set, and WriteTimeout is not, because it
// would cut off a Server-Sent Events stream that is still delivering a model's answer.
func TestServerTimeouts(t *testing.T) {
	srv := newServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("read-side timeouts = header %v, read %v, idle %v; want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want none: it would end a long SSE response", srv.WriteTimeout)
	}
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Errorf("ReadTimeout %v is shorter than ReadHeaderTimeout %v", srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
}

// TestReadAndIdleTimeoutsLeaveAStreamAlone shortens the read and idle timeouts far below the time a response
// streams for, and checks that the stream still ends normally.
func TestReadAndIdleTimeoutsLeaveAStreamAlone(t *testing.T) {
	srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 6 {
			_, _ = w.Write([]byte("data: tick\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: end\n\n"))
	}))
	srv.ReadTimeout, srv.IdleTimeout, srv.ReadHeaderTimeout = 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var last string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			last = sc.Text()
		}
	}
	if last != "data: end" {
		t.Errorf("the stream ended at %q (%v), want it to reach its last event", last, sc.Err())
	}
}
