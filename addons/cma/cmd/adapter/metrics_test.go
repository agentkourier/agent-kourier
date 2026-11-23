package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// serve registers the adapter's own metrics and hands them to the executor: /metrics serves the sessions gauge, and a
// stored session that CMA no longer lists, which the leader forgets as it restores, is counted as gone.
func TestServeServesTheAdaptersOwnMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cma.db")
	st, err := store.OpenSQLite(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSession(t.Context(), store.Session{ContextID: "c-gone", SessionID: "sesn_gone", LastActive: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cma := &fakeCMA{released: make(chan struct{})}
	cma.release()
	api := httptest.NewServer(cma)
	t.Cleanup(api.Close)
	t.Setenv("ANTHROPIC_BASE_URL", api.URL)
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")

	addr := freeAddr(t)
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"LISTEN_ADDR": addr, "SQLITE_PATH": path}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfg, telemetry.Config{}, slog.New(slog.DiscardHandler), serveOptions{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return after its context ended")
		}
	})
	base, p := "http://"+addr, newProber()
	p.waitFor(t, base+"/readyz", http.StatusOK)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, series := range []string{"agentkourier_cma_sessions 0", `agentkourier_cma_sessions_archived_total{reason="gone"} 1`} {
		if !strings.Contains(string(body), series+"\n") {
			t.Errorf("/metrics does not serve %s:\n%s", series, body)
		}
	}
}
