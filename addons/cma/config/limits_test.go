package config

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the default of the adapter's own setting, TOOL_RESULT_TIMEOUT, to docs/reference/limits.md and
// the add-on's reference page. The defaults of the shared settings are the kit's, which its tests hold.
func TestLimitsPage(t *testing.T) {
	c, err := parse(env(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	limitsdoc.Duration(t, "cma.tool-result-timeout", c.ToolResultTimeout)
}
