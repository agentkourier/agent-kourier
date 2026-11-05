package chat

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the mark of a cut and to the fixed limits of a bot's mention.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Text(t, "truncatedMark", Truncated)

	limits := botMentionLimits()
	limitsdoc.Count(t, "botRuns", int(limits.RateLimit.MaxRuns))
	limitsdoc.Duration(t, "botPer", limits.RateLimit.Per.Duration)
	limitsdoc.Count(t, "botConcurrent", int(limits.RateLimit.MaxConcurrent))
	limitsdoc.Count(t, "botPerDay", int(*limits.MaxRunsPerDay))
}
