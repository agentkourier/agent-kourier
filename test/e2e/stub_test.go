package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStubReleasePostsTheEscapedGate(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	if err := NewStub(srv.URL+"/").Release(t.Context(), "run 1/a"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/_control/release/run%201%2Fa" {
		t.Errorf("sent %s %s, want POST /_control/release/run%%201%%2Fa", method, path)
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
