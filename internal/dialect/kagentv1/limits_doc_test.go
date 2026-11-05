package kagentv1

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of this dialect.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "responseStart", DefaultResponseTimeout)
	limitsdoc.Count(t, "questions", maxQuestions)
	limitsdoc.Count(t, "choices", maxChoices)
	limitsdoc.Count(t, "tools", maxTools)
	limitsdoc.Count(t, "actorRunes", maxActorRunes)
}
