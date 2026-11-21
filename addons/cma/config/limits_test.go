package config

import (
	"strconv"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds the defaults that Parse fills in to docs/reference/limits.md. It parses the Postgres store, whose
// leader election the lease timings pace, with none of the timings set.
func TestLimitsPage(t *testing.T) {
	c, err := Parse(env(map[string]string{
		"STORE": "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/cma",
		"ADVERTISE_URL": "http://10.0.0.7:8090", "POD_NAME": "cma-0",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	limitsdoc.Duration(t, "cma.session-idle-ttl", c.SessionIdleTTL)
	limitsdoc.Duration(t, "cma.tool-result-timeout", c.ToolResultTimeout)
	limitsdoc.Duration(t, "cma.task-retention", c.TaskRetention)
	limitsdoc.Duration(t, "cma.lease-duration", c.Lease.Duration)
	limitsdoc.Duration(t, "cma.lease-renew-deadline", c.Lease.RenewDeadline)
	limitsdoc.Duration(t, "cma.lease-retry-period", c.Lease.RetryPeriod)
	limitsdoc.Text(t, "cma.lease-jitter", strconv.FormatFloat(jitterFactor, 'f', -1, 64))
}
