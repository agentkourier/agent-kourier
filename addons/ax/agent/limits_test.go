package agent

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/auth/oidc"
	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// TestLimitsPage holds the kit runtime's timings, which the agent runs with, to the documentation: how often the upkeep
// sweeps the store, the wait for a request's headers, for the requests in flight at a graceful stop, for the turns
// still running after them, and for the hand-over of the lease after those.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.upkeep-interval", runtime.UpkeepInterval)
	limitsdoc.Duration(t, "ax.read-header-timeout", runtime.ReadHeaderTimeout)
	limitsdoc.Duration(t, "ax.shutdown-timeout", runtime.ShutdownTimeout)
	limitsdoc.Duration(t, "ax.turn-stop-timeout", runtime.TurnStopTimeout)
	limitsdoc.Duration(t, "ax.release-timeout", runtime.ReleaseTimeout)
}

// TestForwardLimits holds how long a standby waits to connect to the leader before it answers 503, the kit's router's
// constant, to the documentation.
func TestForwardLimits(t *testing.T) {
	limitsdoc.Duration(t, "ax.forward-dial-timeout", cluster.ForwardDialTimeout)
}

// TestCallerCheckLimits holds how often the agent reads its token file, which it watches at the kit's interval, and how
// long an OIDC discovery or key fetch may take, to the documentation.
func TestCallerCheckLimits(t *testing.T) {
	limitsdoc.Duration(t, "ax.token-reload", auth.ReloadInterval)
	limitsdoc.Duration(t, "ax.oidc-timeout", oidc.Timeout)
}
