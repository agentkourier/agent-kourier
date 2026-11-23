package adapter

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// TestLimitsPage holds the adapter's limits to docs/reference/limits.md: the cut of a tool result and the keepalive
// that an executor with no Keepalive set (as Addon builds it) sends after.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "cma.tool-result-runes", maxToolResultLength)
	limitsdoc.Duration(t, "cma.keepalive", (&Executor{}).keepalive())
}

// TestRuntimeLimits holds the kit runtime's timings, which the adapter runs with, to docs/reference/limits.md: how often
// the upkeep sweeps the store, the wait for a request's headers, for the requests in flight at a graceful stop, for
// the turns still running after them, and for the hand-over of the lease after those.
func TestRuntimeLimits(t *testing.T) {
	limitsdoc.Duration(t, "cma.reap-interval", runtime.UpkeepInterval)
	limitsdoc.Duration(t, "cma.read-header-timeout", runtime.ReadHeaderTimeout)
	limitsdoc.Duration(t, "cma.shutdown-grace", runtime.ShutdownTimeout)
	limitsdoc.Duration(t, "cma.turn-stop-timeout", runtime.TurnStopTimeout)
	limitsdoc.Duration(t, "cma.release-timeout", runtime.ReleaseTimeout)
}
