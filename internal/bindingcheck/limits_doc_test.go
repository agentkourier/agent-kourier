package bindingcheck

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how the Bindings are probed. The broker sets none of these
// Options in production, so the defaults are the values.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "probeInterval", defaultInterval)
	limitsdoc.Duration(t, "probeTimeout", defaultTimeout)
	limitsdoc.Count(t, "probeConcurrency", defaultConcurrency)
}
