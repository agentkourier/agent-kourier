package telemetry

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the telemetry's bounds, which apply to every add-on, to docs/reference/limits.md and the add-ons'
// reference pages: the cap on an error's text in a log line, the cut of a span attribute's value, and how often, and
// for how many kinds, the exporter's and the SDK's diagnostics are logged.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "kit.log-error-bytes", MaxLogErrorBytes)
	limitsdoc.Count(t, "kit.span-attribute-length", maxAttributeValueLength)
	limitsdoc.Duration(t, "kit.diagnostic-log-interval", warnEvery)
	limitsdoc.Count(t, "kit.diagnostic-kinds", maxWarnKeys)
}
