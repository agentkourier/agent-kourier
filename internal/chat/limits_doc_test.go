package chat

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the mark of a cut.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Text(t, "truncatedMark", Truncated)
}
