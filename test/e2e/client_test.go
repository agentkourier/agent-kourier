package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientDecodesAJSONAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/x" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("got %s %s (%s)", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(`{"n":7}`))
	}))
	defer srv.Close()
	var out struct{ N int }
	if err := newClient(srv.URL+"/").do(context.Background(), http.MethodPut, "/x", map[string]int{"a": 1}, &out); err != nil || out.N != 7 {
		t.Fatalf("do = %v, %+v", err, out)
	}
}

func TestClientReportsANon2xxStatusWithTheBodyCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("x", 2*maxErrBody), http.StatusTeapot)
	}))
	defer srv.Close()
	var se *StatusError
	err := newClient(srv.URL).do(context.Background(), http.MethodGet, "/x", nil, nil)
	if !errors.As(err, &se) || se.Status != http.StatusTeapot || len(se.Body) != maxErrBody {
		t.Fatalf("do = %v, want a 418 *StatusError with a body cut to %d bytes", err, maxErrBody)
	}
}

func TestClientReportsATransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	var se *StatusError
	if err := newClient(srv.URL).do(context.Background(), http.MethodGet, "/x", nil, nil); err == nil || errors.As(err, &se) {
		t.Fatalf("do = %v, want a transport error", err)
	}
}
