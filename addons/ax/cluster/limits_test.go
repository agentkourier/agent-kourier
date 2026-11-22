package cluster

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/limitsdoc"
)

// TestLimitsPage holds how long a standby waits to connect to the leader before it answers 503 to the documentation.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.forward-dial-timeout", forwardDialTimeout)
}
