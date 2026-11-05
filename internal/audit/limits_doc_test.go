package audit

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how much of an unusable pause's reason the audit log keeps.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "auditReason", maxReason)
}
