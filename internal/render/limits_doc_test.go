package render

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to what a prompt, a thread context and a status line keep.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "valueBytes", maxNameBytes)
	limitsdoc.Count(t, "keyBytes", maxKeyBytes)
	limitsdoc.Count(t, "linkBytes", maxLinkBytes)
	limitsdoc.Count(t, "instructionUnits", MaxInstructionUnits)
	limitsdoc.Count(t, "fieldUnits", MaxFieldUnits)
	limitsdoc.Count(t, "untrustedUnits", MaxUntrustedUnits)
	limitsdoc.Count(t, "promptUnits", MaxPromptUnits)

	limitsdoc.Count(t, "toolName", maxToolName)
	limitsdoc.Count(t, "toolArgs", maxToolArgs)

	limitsdoc.Count(t, "threadReplies", MaxThreadReplies)
	limitsdoc.Count(t, "threadReplyUnits", MaxThreadReplyUnits)
	limitsdoc.Count(t, "threadNameUnits", MaxThreadNameUnits)
	limitsdoc.Count(t, "threadBlockUnits", MaxThreadBlockUnits)
}
