package runtime

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the runtime's timings, which every add-on runs with, to docs/reference/limits.md and the
// add-ons' reference pages: how often the upkeep sweeps the store, the wait for a request's headers, for the requests
// in flight at a graceful stop, for the turns still running after them, and for the hand-over of the lease after those,
// and the bound on a backend's release of an idle context.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "kit.upkeep-interval", UpkeepInterval)
	limitsdoc.Duration(t, "kit.read-header-timeout", ReadHeaderTimeout)
	limitsdoc.Duration(t, "kit.shutdown-timeout", ShutdownTimeout)
	limitsdoc.Duration(t, "kit.turn-stop-timeout", TurnStopTimeout)
	limitsdoc.Duration(t, "kit.release-timeout", ReleaseTimeout)
	limitsdoc.Duration(t, "kit.context-release-timeout", ContextReleaseTimeout)
}
