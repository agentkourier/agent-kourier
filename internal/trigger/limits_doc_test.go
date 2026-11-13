package trigger

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the head of a field that a trigger reads, and to the fixed limits
// of a bot's mention.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "matchedField", maxMatchedFieldBytes)

	limits := botMentionLimits()
	limitsdoc.Count(t, "botRuns", int(limits.RateLimit.MaxRuns))
	limitsdoc.Duration(t, "botPer", limits.RateLimit.Per.Duration)
	limitsdoc.Count(t, "botConcurrent", int(limits.RateLimit.MaxConcurrent))
	limitsdoc.Count(t, "botPerDay", int(*limits.MaxRunsPerDay))
	limitsdoc.Count(t, "toldLimited", maxToldLimited)
}
