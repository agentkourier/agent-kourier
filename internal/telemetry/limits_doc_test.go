package telemetry

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how much of an error a log line carries.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "logError", MaxLogErrorBytes)
}
