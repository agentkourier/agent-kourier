package config

import (
	"strconv"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the defaults that Parse fills in to the documentation. It parses the Postgres store, whose leader
// election the lease timings pace, with none of the timings set.
func TestLimitsPage(t *testing.T) {
	env := map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "m", "AUTH_TOKEN_FILE": "/tokens",
		"STORE": "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/ax", "ADVERTISE_URL": "http://10.0.0.7:8090", "POD_NAME": "ax-0"}
	c, err := Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	limitsdoc.Duration(t, "ax.task-retention", c.TaskRetention)
	limitsdoc.Duration(t, "ax.session-idle-ttl", c.SessionIdleTTL)
	limitsdoc.Duration(t, "ax.lease-duration", c.Lease.Duration)
	limitsdoc.Duration(t, "ax.lease-renew-deadline", c.Lease.RenewDeadline)
	limitsdoc.Duration(t, "ax.lease-retry-period", c.Lease.RetryPeriod)
	limitsdoc.Text(t, "ax.lease-jitter", strconv.FormatFloat(jitterFactor, 'f', -1, 64))
}
