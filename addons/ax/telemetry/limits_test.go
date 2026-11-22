package telemetry

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/limitsdoc"
)

// TestLimitsPage holds the cap on an error's text in a log line to the documentation.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "ax.log-error-bytes", MaxLogErrorBytes)
}
