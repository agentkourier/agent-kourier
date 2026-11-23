package serve

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the server's timeouts to the documentation: the wait for a request's headers, for the requests in
// flight at a graceful stop, and for the hand-over of the lease after them.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.read-header-timeout", readHeaderTimeout)
	limitsdoc.Duration(t, "ax.shutdown-timeout", ShutdownTimeout)
	limitsdoc.Duration(t, "ax.release-timeout", releaseTimeout)
}

// TestForwardLimits holds how long a standby waits to connect to the leader before it answers 503, the kit's router's
// constant, to the documentation.
func TestForwardLimits(t *testing.T) {
	limitsdoc.Duration(t, "ax.forward-dial-timeout", cluster.ForwardDialTimeout)
}
