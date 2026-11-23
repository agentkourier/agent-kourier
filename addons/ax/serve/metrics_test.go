package serve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// newTelemetry is the agent's telemetry, as the command builds it, recording no spans.
func newTelemetry(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(agent.TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// served reports whether tel's /metrics serves series with the value want.
func served(t *testing.T, tel *telemetry.Telemetry, series, want string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return strings.Contains(rec.Body.String(), series+" "+want+"\n")
}

// The labels of agentkourier_ax_auth_refused_total are the auth package's reasons: a refusal the check reports is
// counted under its own name.
func TestAuthRefusalsAreCountedUnderTheChecksReasons(t *testing.T) {
	env := map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model", "AUTH_DISABLED": "true"}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	tel := newTelemetry(t)
	check, _, err := callerCheck(cfg, slog.New(slog.DiscardHandler), tel)
	if err != nil {
		t.Fatal(err)
	}
	reasons := []auth.Reason{auth.ReasonMissing, auth.ReasonInvalid, auth.ReasonUnavailable}
	for _, r := range reasons {
		check.Refused(r)
	}
	for _, r := range reasons {
		if series := `agentkourier_ax_auth_refused_total{reason="` + string(r) + `"}`; !served(t, tel, series, "1") {
			t.Errorf("%s is not served at 1", series)
		}
	}
}

// The upkeep Run builds counts the idle contexts it deletes in the agent's metrics.
func TestTheUpkeepCountsTheSessionsItDeletes(t *testing.T) {
	env := map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model", "AUTH_DISABLED": "true", "SESSION_IDLE_TTL": "30m"}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "ax.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Create(caller(), &a2a.Task{ID: "idle", ContextID: "c-idle", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}); err != nil {
		t.Fatal(err)
	}
	tel := newTelemetry(t)
	upkeep, err := newUpkeep(cfg, st, slog.New(slog.DiscardHandler), tel)
	if err != nil {
		t.Fatal(err)
	}
	upkeep.Now = func() time.Time { return time.Now().Add(time.Hour) }
	upkeep.Sweep(t.Context())
	if series := `agentkourier_ax_sessions_deleted_total{reason="idle"}`; !served(t, tel, series, "1") {
		t.Fatalf("%s is not served at 1", series)
	}
}

// Run registers the agent's own metrics in the Telemetry it is given, which the command builds and serves.
func TestRunRegistersTheAgentsOwnMetrics(t *testing.T) {
	addr := freeAddr(t)
	env := map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model", "AUTH_DISABLED": "true", "LISTEN_ADDR": addr,
		"SQLITE_PATH": filepath.Join(t.TempDir(), "ax.db")}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	tel := newTelemetry(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, echo{}, Options{Log: slog.New(slog.DiscardHandler), Telemetry: tel, Exit: func(int) {}})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	waitFor(t, addr+" to be ready", func() bool { c, _ := probe(http.MethodGet, "http://"+addr+"/readyz", ""); return c == http.StatusOK })
	again := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_sessions_deleted_total",
		Help: "ADK sessions the agent deleted, each with its context's tasks, by reason: idle (no activity for SESSION_IDLE_TTL)."},
		[]string{"reason"})
	var already prometheus.AlreadyRegisteredError
	if err := tel.Register(again); !errors.As(err, &already) {
		t.Fatalf("registering agentkourier_ax_sessions_deleted_total again = %v, want AlreadyRegisteredError: Run did not register it", err)
	}
}
