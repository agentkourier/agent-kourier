package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSignalerSendsEachVerbForTheContainer(t *testing.T) {
	var method, path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	sig := NewSignaler(srv.URL + "/")
	for _, tc := range []struct {
		verb string
		call func(string) error
	}{
		{"stop", func(c string) error { return sig.Stop(t.Context(), c) }},
		{"cont", func(c string) error { return sig.Cont(t.Context(), c) }},
		{"kill", func(c string) error { return sig.Kill(t.Context(), c) }},
	} {
		if err := tc.call("abc123"); err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if method != http.MethodPost || path != "/"+tc.verb || query != "container=abc123" {
			t.Errorf("%s sent %s %s?%s; want POST /%s?container=abc123", tc.verb, method, path, query, tc.verb)
		}
	}
}

func TestSignalerReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	err := NewSignaler(srv.URL).Stop(t.Context(), "abc")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("Stop = %v; want a 404 *StatusError, wrapped with what was asked", err)
	}
}

func TestSignalerReadyAcceptsAnyAnswerAndReportsNone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler()) // the signaler has no route at /, and a 404 shows it is there
	if err := NewSignaler(srv.URL).Ready(t.Context()); err != nil {
		t.Errorf("Ready of a server that answers = %v; want nil", err)
	}
	srv.Close()
	if err := NewSignaler(srv.URL).Ready(t.Context()); err == nil {
		t.Error("Ready of a server that is gone = nil; want an error")
	}
}
