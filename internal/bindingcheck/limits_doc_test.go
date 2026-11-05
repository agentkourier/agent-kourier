package bindingcheck

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how the Bindings are probed. The broker leaves the Options' interval
// and concurrency to these defaults and its own wiring sets the timeout (the broker's test guards that one).
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "probeInterval", defaultInterval)
	limitsdoc.Count(t, "probeConcurrency", defaultConcurrency)
	limitsdoc.Count(t, "probeJitter", int(jitterFraction*100))
}
