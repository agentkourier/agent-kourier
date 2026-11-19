package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWebhookVendorAPIServesTheRecipesAgents: with -webhook-vendor-api the stub serves the webhook recipes' agent as agents/<vendor> for each of
// the four vendors, and a prompt of a recipe's template makes that agent post on the vendor API it was given; without the flag there is no
// such agent.
func TestWebhookVendorAPIServesTheRecipesAgents(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+string(body))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	t.Cleanup(api.Close)

	plain := serve(t, 0, false)
	stub := newStub(0, false, "", api.URL)
	srv := httptest.NewServer(withControl(stub))
	t.Cleanup(srv.Close)
	t.Cleanup(stub.Close)
	for _, vendor := range webhookVendors {
		card := "/agents/" + webhookAgentNamespace + "/" + vendor + "/.well-known/agent-card.json"
		if got := statusOf(t.Context(), t, http.MethodGet, srv.URL+card); got != http.StatusOK {
			t.Errorf("with a vendor API, the card of %s answered %d; want 200", vendor, got)
		}
		if got := statusOf(t.Context(), t, http.MethodGet, plain.URL+card); got != http.StatusNotFound {
			t.Errorf("without a vendor API, the card of %s answered %d; want 404", vendor, got)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	prompt := `GitHub pull request #42 in repository acme/widgets needs a review.\nHead commit 9f8e7d6c5b4a39281706f5e4d3c2b1a098765432, base commit 0123456.`
	send := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"` + prompt + `"}]}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agents/"+webhookAgentNamespace+"/github", strings.NewReader(send))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "/vendor/github/repos/acme/widgets/pulls/42/reviews ") ||
		!strings.Contains(calls[0], "Review of acme/widgets#42 at 9f8e7d6c5b4a39281706f5e4d3c2b1a098765432 (turn 1)") {
		t.Errorf("the vendor API was called %q; want the review of acme/widgets#42 at its head commit, once", calls)
	}
}

func TestVendorAPIMustBeAnAbsoluteHTTPURL(t *testing.T) {
	for _, tc := range []struct {
		api string
		ok  bool
	}{
		{"", true},
		{"http://fakevendors.e2e.svc.cluster.local:8080", true},
		{"https://vendors.test", true},
		{"fakevendors:8080", false},
		{"ftp://vendors.test", false},
		{"http://", false},
		{"http://vendors.test/vendor", false},
	} {
		if err := checkVendorAPI(tc.api); (err == nil) != tc.ok {
			t.Errorf("checkVendorAPI(%q) = %v; want ok %v", tc.api, err, tc.ok)
		}
	}
}
