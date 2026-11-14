package live

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how many steps a live reply shows.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "steps", maxSteps)
	limitsdoc.Count(t, "stepsListed", maxListed)
}

// TestLimitsPageHoldsTheWaitForADownConnection holds the page to how often a write is tried again and how long the final text is
// waited for while its connection is down.
func TestLimitsPageHoldsTheWaitForADownConnection(t *testing.T) {
	limitsdoc.Duration(t, "renderRetry", defaultThrottle)
	limitsdoc.Duration(t, "renderFinal", defaultFinalDeadline)
}
