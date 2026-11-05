package slack

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of Slack that the adapter lays a message out by.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "replyUnits", maxMarkdown)
	limitsdoc.Count(t, "stepText", maxStepText)
	limitsdoc.Count(t, "blocks", maxBlocks)
	limitsdoc.Count(t, "inlineChoices", maxInlineOptions)
	limitsdoc.Count(t, "selectChoices", maxSelectOptions)
	limitsdoc.Count(t, "choiceUnits", maxOption)
	limitsdoc.Count(t, "listLine", maxListLine)
	limitsdoc.Count(t, "headerUnits", maxHeader)
	limitsdoc.Count(t, "approvalArgs", maxApprovalArgs)
	limitsdoc.Count(t, "approvalTotal", maxApprovalTotal)
	limitsdoc.Count(t, "approvalBlocks", maxApprovalBlocks)
}
