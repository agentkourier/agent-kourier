package adapter

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the adapter's limits to docs/reference/limits.md: the cut of a tool result and the keepalive
// that an executor with no Keepalive set (as Addon builds it) sends after. The limits every add-on shares are the
// kit's, which its tests hold.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "cma.tool-result-runes", maxToolResultLength)
	limitsdoc.Duration(t, "cma.keepalive", (&Executor{}).keepalive())
}
