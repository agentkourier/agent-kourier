package chat

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the mark of a cut, and to the summary of a tool's arguments that
// the ending of an approval shows.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Text(t, "truncatedMark", Truncated)
	limitsdoc.Count(t, "toolSummary", MaxToolSummary)
}
