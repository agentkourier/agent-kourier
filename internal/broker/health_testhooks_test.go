//go:build testhooks

package broker

import (
	"net/http"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

func TestTesthooksHandlerServesThePersistedAuditRoute(t *testing.T) {
	audit := testhooks.NewAuditEndpoint()
	code, body := get(t, newHandler(mustMetrics(t, nil), &Readiness{}, audit), "/__testhooks/audit")
	if code != http.StatusServiceUnavailable || body == "" {
		t.Errorf("testhooks audit route = %d %q; want 503 until Run connects the store", code, body)
	}
}
