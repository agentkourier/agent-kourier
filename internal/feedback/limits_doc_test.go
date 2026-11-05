package feedback_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how many alert names a Binding has a series for in the vote metric.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "feedbackLabels", feedback.MaxAlertLabels)
}
