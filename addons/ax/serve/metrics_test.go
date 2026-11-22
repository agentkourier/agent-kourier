package serve

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/auth"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// The labels of agentkourier_ax_auth_refused_total are the auth package's reasons: a refusal the check reports is
// counted under its own name.
func TestAuthRefusalsAreCountedUnderTheChecksReasons(t *testing.T) {
	env := map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model", "AUTH_DISABLED": "true"}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	tel, err := telemetry.New(agent.TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	check, _, err := callerCheck(cfg, slog.New(slog.DiscardHandler), tel)
	if err != nil {
		t.Fatal(err)
	}
	reasons := []string{auth.ReasonMissing, auth.ReasonInvalid, auth.ReasonUnavailable}
	for _, r := range reasons {
		check.Refused(r)
	}
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for _, r := range reasons {
		if series := `agentkourier_ax_auth_refused_total{reason="` + r + `"} 1`; !strings.Contains(rec.Body.String(), series+"\n") {
			t.Errorf("%s is not served", series)
		}
	}
}
