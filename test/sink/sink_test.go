package sink_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/test/sink"
)

func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
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
	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/alertmanager", `{"status":"firing"}`},
		{http.MethodPost, "/", `{"status":"resolved"}`},
		{http.MethodPut, "/other?x=1", "third"},
	} {
		if rec := do(t, h, c.method, c.target, c.body); rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200", c.method, c.target, rec.Code)
		}
	}
	want := []sink.Request{
		{Method: "POST", Path: "/alertmanager", Body: `{"status":"firing"}`},
		{Method: "POST", Path: "/", Body: `{"status":"resolved"}`},
		{Method: "PUT", Path: "/other", Query: "x=1", Body: "third"},
	}
	got := recorded(t, h)
	if len(got) != len(want) {
		t.Fatalf("recorded %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
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
		method, target, body string
		want                 int
	}{
		{http.MethodPost, "/requests", "x", http.StatusMethodNotAllowed},
		{http.MethodGet, "/alertmanager", "", http.StatusNotFound},
		{http.MethodPost, "/", strings.Repeat("x", sink.MaxBody+1), http.StatusRequestEntityTooLarge},
	} {
		h := sink.NewHandler()
		if rec := do(t, h, c.method, c.target, c.body); rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.target, rec.Code, c.want)
		}
		if got := recorded(t, h); len(got) != 0 {
			t.Errorf("%s %s was recorded: %+v", c.method, c.target, got)
		}
	}
}
