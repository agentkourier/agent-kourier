package anthropic

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the token limit of a reply and the SDK's retries to the documentation.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "ax.anthropic-max-tokens", DefaultMaxTokens)
	limitsdoc.Count(t, "ax.anthropic-retries", MaxRetries)
}
