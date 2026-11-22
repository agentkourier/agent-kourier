package agent

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds how often the upkeep sweeps the store to the documentation.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.upkeep-interval", UpkeepInterval)
}
