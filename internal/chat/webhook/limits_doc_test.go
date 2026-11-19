package webhook

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds docs/reference/limits.md to the bounds of a webhook connection.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "webhookAckWait", ackWait)
	limitsdoc.Duration(t, "webhookBusyRetryAfter", busyRetryAfter)
	limitsdoc.Count(t, "webhookDepth", maxDepth)
	limitsdoc.Count(t, "webhookIDValue", maxIDValue)
	limitsdoc.Count(t, "webhookDeliveryValue", maxIDValue)
	limitsdoc.Count(t, "webhookTokenLen", minTokenLen)
	limitsdoc.Count(t, "webhookPointerLen", maxPointerLen)
	limitsdoc.Duration(t, "webhookUnverifiedLog", unverifiedLogEvery)
}
