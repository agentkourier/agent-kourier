package sink_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/test/sink"
)

func send(t *testing.T, h http.Handler, method, target, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, h, method, target, body, nil)
}

// recorded reads the sink's log through its own endpoint.
func recorded(t *testing.T, h http.Handler) []sink.Request {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/requests", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /requests = %d, want 200", rec.Code)
	}
	var got []sink.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET /requests body %q is not a JSON list of requests: %v", rec.Body, err)
	}
	return got
}

func TestPostIsRecordedInOrderAndAnswered200(t *testing.T) {
	h := sink.NewHandler()
	var want []string
	for _, c := range []struct{ method, target, body, path, query string }{
		{http.MethodPost, "/alertmanager", `{"status":"firing"}`, "/alertmanager", ""},
		{http.MethodPost, "/", `{"status":"resolved"}`, "/", ""},
		{http.MethodPut, "/other?x=1", "third", "/other", "x=1"},
	} {
		if rec := do(t, h, c.method, c.target, c.body); rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200", c.method, c.target, rec.Code)
		}
		want = append(want, fmt.Sprint(c.method, c.path, c.query, c.body))
	}
	got := recorded(t, h)
	if len(got) != len(want) {
		t.Fatalf("recorded %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i, r := range got {
		if have := fmt.Sprint(r.Method, r.Path, r.Query, r.Body); have != want[i] {
			t.Errorf("request %d = %q, want %q", i, have, want[i])
		}
	}
}

func TestHeadersAreRecorded(t *testing.T) {
	h := sink.NewHandler()
	send(t, h, http.MethodPost, "/", "{}", http.Header{"Content-Type": {"application/json"}, "User-Agent": {"Alertmanager/0.28"}})
	got := recorded(t, h)
	if len(got) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(got))
	}
	if ct, ua := got[0].Header.Get("Content-Type"), got[0].Header.Get("User-Agent"); ct != "application/json" || ua != "Alertmanager/0.28" {
		t.Errorf("recorded headers Content-Type=%q User-Agent=%q, want application/json and Alertmanager/0.28", ct, ua)
	}
}

func TestEmptyLogIsAnEmptyList(t *testing.T) {
	rec := do(t, sink.NewHandler(), http.MethodGet, "/requests", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("GET /requests on a new sink = %q, want []", got)
	}
}

func TestDeleteClearsTheLog(t *testing.T) {
	h := sink.NewHandler()
	do(t, h, http.MethodPost, "/", "a")
	if rec := do(t, h, http.MethodDelete, "/requests", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /requests = %d, want 204", rec.Code)
	}
	if got := recorded(t, h); len(got) != 0 {
		t.Fatalf("after DELETE, recorded = %+v, want none", got)
	}
}

func TestOtherRequestsAreRefusedAndNotRecorded(t *testing.T) {
	for _, c := range []struct {
		method, target string
		want           int
	}{
		{http.MethodPost, "/requests", http.StatusMethodNotAllowed},
		{http.MethodGet, "/alertmanager", http.StatusNotFound},
	} {
		h := sink.NewHandler()
		rec := do(t, h, c.method, c.target, "x")
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.target, rec.Code, c.want)
		}
		if c.want == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != "GET, DELETE" {
			t.Errorf("%s %s: Allow = %q, want %q", c.method, c.target, rec.Header().Get("Allow"), "GET, DELETE")
		}
		if got := recorded(t, h); len(got) != 0 {
			t.Errorf("%s %s was recorded: %+v", c.method, c.target, got)
		}
	}
}

// The limit is part of the sink's contract (tests size fixtures against it): exactly MaxBody
// bytes are recorded whole, one more is refused and not recorded.
func TestBodyLimitIsOneMiB(t *testing.T) {
	if sink.MaxBody != 1<<20 {
		t.Fatalf("MaxBody = %d, want %d", sink.MaxBody, 1<<20)
	}
	h := sink.NewHandler()
	full := strings.Repeat("x", sink.MaxBody)
	if rec := do(t, h, http.MethodPost, "/", full); rec.Code != http.StatusOK {
		t.Fatalf("POST of MaxBody bytes = %d, want 200", rec.Code)
	}
	if got := recorded(t, h); len(got) != 1 || got[0].Body != full {
		t.Fatalf("a body of MaxBody bytes was not recorded intact (recorded %d requests)", len(got))
	}
	if rec := do(t, h, http.MethodPost, "/", full+"x"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST of MaxBody+1 bytes = %d, want 413", rec.Code)
	}
	if got := recorded(t, h); len(got) != 1 {
		t.Errorf("recorded %d requests after the refused one, want still 1", len(got))
	}
}

// Alertmanager retries and several receivers post at once; none may be lost. Run with -race.
func TestConcurrentPostsAreAllRecorded(t *testing.T) {
	const posts = 64
	h := sink.NewHandler()
	var wg sync.WaitGroup
	for i := range posts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(t, h, http.MethodPost, "/", fmt.Sprintf("body-%d", i))
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, r := range recorded(t, h) {
		seen[r.Body] = true
	}
	if len(seen) != posts {
		t.Fatalf("recorded %d distinct bodies, want %d", len(seen), posts)
	}
}
