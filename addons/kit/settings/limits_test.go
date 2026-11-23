package settings

import (
	"strconv"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the defaults that Parse fills in for every add-on to docs/reference/limits.md and the add-ons'
// reference pages: how long a settled task and an idle context are kept, the lease's timings, and the stretch of the
// retry period that the renew deadline must exceed. It parses the Postgres store, whose leader election the timings
// pace, with none of them set.
func TestLimitsPage(t *testing.T) {
	c, err := parse(env(pg(nil)), tokenSpec)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	limitsdoc.Duration(t, "kit.task-retention", c.TaskRetention)
	limitsdoc.Duration(t, "kit.session-idle-ttl", c.SessionIdleTTL)
	limitsdoc.Duration(t, "kit.lease-duration", c.Lease.Duration)
	limitsdoc.Duration(t, "kit.lease-renew-deadline", c.Lease.RenewDeadline)
	limitsdoc.Duration(t, "kit.lease-retry-period", c.Lease.RetryPeriod)
	limitsdoc.Text(t, "kit.lease-jitter", strconv.FormatFloat(LeaseJitter, 'f', -1, 64))
}
