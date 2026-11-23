package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

// The adapter registers its own metrics in the telemetry the runtime serves and hands them to the executor: /metrics serves the sessions gauge, and a
// stored session that CMA no longer lists, which the leader forgets as it restores, is counted as gone.
func TestTheAdaptersOwnMetricsAreServed(t *testing.T) {
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

	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"LISTEN_ADDR": freeAddr(t), "SQLITE_PATH": path}
	base, p := start(t, env, io.Discard), newProber()
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
