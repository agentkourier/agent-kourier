package adapter

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds the adapter's limits to docs/reference/limits.md: the cut of a tool result, the keepalive that an
// executor with no Keepalive set (as the adapter's command builds it) sends after, and how often the reaper runs.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "cma.tool-result-runes", maxToolResultLength)
	limitsdoc.Duration(t, "cma.keepalive", (&Executor{}).keepalive())
	limitsdoc.Duration(t, "cma.reap-interval", reapInterval)
}
