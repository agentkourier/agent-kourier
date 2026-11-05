package broker

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the broker's wiring.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "leaseDuration", defaultLeaseDuration)
	limitsdoc.Duration(t, "renewDeadline", defaultRenewDeadline)
	limitsdoc.Duration(t, "retryPeriod", defaultRetryPeriod)
	limitsdoc.Duration(t, "syncTimeout", defaultSyncTimeout)
	limitsdoc.Duration(t, "shutdown", shutdownTotal)

	limitsdoc.Count(t, "secretsQPS", secretsQPS)
	limitsdoc.Count(t, "secretsBurst", secretsBurst)
	limitsdoc.Count(t, "statusQPS", statusQPS)
	limitsdoc.Count(t, "statusBurst", statusBurst)
	limitsdoc.Count(t, "leaseQPS", leaseQPS)
	limitsdoc.Count(t, "leaseBurst", leaseBurst)

	limitsdoc.Duration(t, "probeGap", minRevisionGap)
	limitsdoc.Count(t, "threadWindow", threadFetchLimit)
	limitsdoc.Count(t, "statusMessage", maxStatusMessage)

	limitsdoc.Duration(t, "sessionGrace", defaultSessionGrace)
	limitsdoc.Duration(t, "strandedReply", defaultStrandedReplyAge)
	limitsdoc.Duration(t, "auditRetentionMin", MinAuditRetention)
}
