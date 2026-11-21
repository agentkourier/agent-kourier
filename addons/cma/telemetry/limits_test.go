package telemetry

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds the cap on an error's text in a log line to docs/reference/limits.md.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "cma.log-error-bytes", MaxLogErrorBytes)
}
