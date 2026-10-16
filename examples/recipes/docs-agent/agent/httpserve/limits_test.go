package httpserve_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/httpserve"
)

// zeros is an endless body of zero bytes, so that sending a huge request costs the test no memory.
type zeros struct{ sent atomic.Int64 }

func (z *zeros) Read(p []byte) (int, error) {
	clear(p)
	z.sent.Add(int64(len(p)))
	return len(p), nil
}

// serveReading runs a server whose handler reads the whole body, and reports what the handler saw.
type seen struct {
	called atomic.Bool
	n      atomic.Int64
	err    atomic.Value // error
}

func serveReading(t *testing.T) (url string, s *seen) {
	t.Helper()
	s = &seen{}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	url, _, err := httpserve.Loopback(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called.Store(true)
		n, err := io.Copy(io.Discard, r.Body)
		s.n.Store(n)
		if err != nil {
			s.err.Store(err)
			http.Error(w, "read: "+err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return url, s
}

func TestRunRefusesABodyDeclaredOverTheCap(t *testing.T) {
	url, s := serveReading(t)
	body := io.LimitReader(&zeros{}, httpserve.MaxBodyBytes+1)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	req.ContentLength = httpserve.MaxBodyBytes + 1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if s.called.Load() {
		t.Error("the handler ran for a body whose declared length is over the cap")
	}
}

func TestRunAcceptsABodyAtTheCap(t *testing.T) {
	url, s := serveReading(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, io.LimitReader(&zeros{}, httpserve.MaxBodyBytes))
	req.ContentLength = httpserve.MaxBodyBytes
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || s.n.Load() != httpserve.MaxBodyBytes {
		t.Errorf("status = %d, handler read %d bytes; want 200 and %d", resp.StatusCode, s.n.Load(), httpserve.MaxBodyBytes)
	}
}

// TestRunCutsAnUnboundedBodyAtTheCap sends a chunked body of 512 MiB, which declares no length, and checks that the
// handler could read no more than the cap and that the server's memory did not follow the body.
func TestRunCutsAnUnboundedBodyAtTheCap(t *testing.T) {
	url, s := serveReading(t)
	const sendUpTo = 512 << 20
	z := &zeros{}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, io.LimitReader(z, sendUpTo))
	req.ContentLength = -1 // chunked

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("status 200 for a body over the cap")
		}
	} // the server may also reset the connection while the client is still sending
	runtime.ReadMemStats(&after)

	if got := s.n.Load(); got > httpserve.MaxBodyBytes {
		t.Errorf("the handler read %d bytes, over the cap of %d", got, httpserve.MaxBodyBytes)
	}
	var tooBig *http.MaxBytesError
	if e, _ := s.err.Load().(error); !errors.As(e, &tooBig) {
		t.Errorf("handler's read error = %v, want *http.MaxBytesError", e)
	}
	if sent := z.sent.Load(); sent >= sendUpTo {
		t.Errorf("the client sent all %d bytes: the server kept reading", sent)
	}
	// Both ends of the exchange run in this process, so allow for the client's own buffers; reading the body whole
	// would have cost the full 512 MiB.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("allocated %d MiB for a body that was cut at %d MiB", grew>>20, httpserve.MaxBodyBytes>>20)
	}
}
