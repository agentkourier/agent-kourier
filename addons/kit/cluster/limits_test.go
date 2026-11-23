package cluster

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds how long a standby waits to connect to the leader before it answers 503 to
// docs/reference/limits.md and the add-ons' reference pages.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "kit.forward-dial-timeout", ForwardDialTimeout)
}
