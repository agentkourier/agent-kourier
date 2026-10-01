package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

func TestStubReleasePostsTheGate(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	if err := NewStub(srv.URL+"/").Release(t.Context(), "run-1_a"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/_control/release/run-1_a" {
		t.Errorf("sent %s %s, want POST /_control/release/run-1_a", method, path)
	}
}

func TestStubReleaseReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	err := NewStub(srv.URL).Release(t.Context(), "g")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("Release = %v, want a 404 *StatusError", err)
	}
}

func TestStubReleaseRefusesAnInvalidGateWithoutSendingIt(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	t.Cleanup(srv.Close)

	for _, gate := range []string{"", "run 1", "a/b", "a:b"} {
		if err := NewStub(srv.URL).Release(t.Context(), gate); err == nil {
			t.Errorf("Release(%q) succeeded, want an invalid-name error", gate)
		}
	}
	if sent {
		t.Error("an invalid gate name reached the stub")
	}
}

func TestStubGatesAreValidAndUniquePerRun(t *testing.T) {
	a, b := NewStub("http://stub"), NewStub("http://stub")
	seen := map[string]bool{}
	for _, name := range []string{"TestS4/queued reply", "TestS4/queued reply", "", "é/ü"} {
		for _, s := range []*Stub{a, b} {
			g := s.Gate(name)
			if !stubagent.ValidGate(g) {
				t.Errorf("Gate(%q) = %q, not a valid gate name", name, g)
			}
			if seen[g] {
				t.Errorf("Gate(%q) = %q again", name, g)
			}
			seen[g] = true
		}
	}
}
