package telemetry

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the cap on an error's text in a log line, which the log handler applies to every add-on's log,
// to docs/reference/limits.md and the add-ons' reference pages.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "kit.log-error-bytes", MaxLogErrorBytes)
}
