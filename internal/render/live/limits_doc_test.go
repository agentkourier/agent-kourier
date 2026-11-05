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
