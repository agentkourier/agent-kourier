package slack

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of Slack that the adapter lays a message out by, and to
// the adapter's own waits. The bounds of the events it holds are those of internal/chat/lanes, whose tests hold the page to them.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "replyUnits", maxMarkdown)
	limitsdoc.Count(t, "stepText", maxStepText)
	limitsdoc.Count(t, "blocks", maxBlocks)
	limitsdoc.Count(t, "inlineChoices", maxInlineOptions)
	limitsdoc.Count(t, "selectChoices", maxSelectOptions)
	limitsdoc.Count(t, "choiceUnits", maxOption)
	limitsdoc.Count(t, "listLine", maxListLine)
	limitsdoc.Count(t, "headerUnits", maxHeader)
	limitsdoc.Count(t, "questionUnits", maxLabel)
	limitsdoc.Count(t, "approvalArgs", maxApprovalArgs)
	limitsdoc.Count(t, "approvalTotal", maxApprovalTotal)
	limitsdoc.Count(t, "approvalBlocks", maxApprovalBlocks)
	limitsdoc.Count(t, "slackToolName", maxToolName)
	limitsdoc.Count(t, "slackToolArgs", maxToolArgs)
	limitsdoc.Duration(t, "slackSignatureAge", signatureMaxAge)
	limitsdoc.Duration(t, "slackHTTPAckWait", httpAckWait)
	limitsdoc.Duration(t, "shedLogEvery", shedLogEvery)
}
