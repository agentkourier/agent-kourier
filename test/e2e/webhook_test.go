package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/test/sink"
)

func TestPostWebhook(t *testing.T) {
	const token = "fake-webhook-token"
	var auth []string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, body = r.Header.Values("Authorization"), nil
		body, _ = io.ReadAll(r.Body)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	resp, err := PostWebhook(context.Background(), srv.URL, token, []byte(`{"a":1}`))
	if err != nil || resp.Status != http.StatusUnauthorized || string(resp.Body) != "nope\n" {
		t.Fatalf("PostWebhook = %+v, %v; a 401 is an answer, not an error", resp, err)
	}
	if len(auth) != 1 || auth[0] != "Bearer "+token || string(body) != `{"a":1}` {
		t.Errorf("server saw %q and %q", auth, body)
	}
	if _, err := PostWebhook(context.Background(), srv.URL, "", nil); err != nil || len(auth) != 0 {
		t.Errorf("without a token: %v, Authorization %q, want none", err, auth)
	}
	srv.Close()
	if _, err := PostWebhook(context.Background(), srv.URL, token, nil); err == nil {
		t.Error("PostWebhook to a closed server succeeded")
	}
}

func TestPostWebhookReachesTheRealSink(t *testing.T) {
	srv := httptest.NewServer(sink.NewHandler())
	defer srv.Close()
	if resp, err := PostWebhook(context.Background(), srv.URL+"/hook", "fake-token", []byte(`{"x":1}`)); err != nil || resp.Status != http.StatusOK {
		t.Fatalf("PostWebhook = %+v, %v", resp, err)
	}
	reqs, err := NewSink(srv.URL).Requests(context.Background())
	if err != nil || len(reqs) != 1 || reqs[0].Path != "/hook" || reqs[0].Body != `{"x":1}` {
		t.Fatalf("sink holds %+v, %v", reqs, err)
	}
}
