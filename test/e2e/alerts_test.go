package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAlertmanagerPost(t *testing.T) {
	status, got := http.StatusOK, []map[string]any(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/alerts" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	am, ctx := NewAlertmanager(srv.URL), context.Background()
	ends := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := am.Post(ctx, Alert{Labels: map[string]string{"alertname": am.Name("Disk")}}, Alert{Labels: map[string]string{"alertname": "B"}, EndsAt: ends}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["labels"].(map[string]any)["alertname"] != am.Name("Disk") || got[1]["endsAt"] != "2030-01-02T03:04:05Z" {
		t.Fatalf("Alertmanager received %v", got)
	}
	if _, set := got[0]["startsAt"]; set {
		t.Error("an unset startsAt must be left out so Alertmanager stamps it")
	}
	if err := am.Post(ctx); err == nil {
		t.Error("Post with no alerts succeeded; nil would encode as null")
	}
	status = http.StatusBadRequest
	var se *StatusError
	if err := am.Post(ctx, Alert{}); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Errorf("Post after a refusal = %v, want a 400 *StatusError", err)
	}
}

func TestAlertNamesAreUniquePerRunAndStableWithinOne(t *testing.T) {
	a, b := NewAlertmanager("http://x"), NewAlertmanager("http://x")
	first, again := a.Name("Burst"), a.Name("Burst")
	if first != again || first == b.Name("Burst") || first == a.Name("Other") {
		t.Errorf("names %q, %q (same client), %q (another run), %q (another base)", first, again, b.Name("Burst"), a.Name("Other"))
	}
}
