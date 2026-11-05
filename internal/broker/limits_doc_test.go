package broker

import (
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the broker's wiring. Each value is the one the
// broker runs with: a default that production leaves unset is read where it is filled in (the election's timings, the retention
// loop's graces), and a constant is the one production reads. The sync timeout is not here: its default is the binary's own
// (cmd/agent-kourier), which that package's test guards.
func TestLimitsPage(t *testing.T) {
	cfg := ElectionConfig{}.withDefaults() // the broker passes zero timings unless the environment sets them
	limitsdoc.Duration(t, "leaseDuration", cfg.LeaseDuration)
	limitsdoc.Duration(t, "renewDeadline", cfg.RenewDeadline)
	limitsdoc.Duration(t, "retryPeriod", cfg.RetryPeriod)
	limitsdoc.Text(t, "leaseJitter", fmt.Sprint(leaderelection.JitterFactor))
	limitsdoc.Duration(t, "shutdown", newShutdown(nil).total)

	limitsdoc.Count(t, "secretsQPS", secretsQPS)
	limitsdoc.Count(t, "secretsBurst", secretsBurst)
	limitsdoc.Count(t, "statusQPS", statusQPS)
	limitsdoc.Count(t, "statusBurst", statusBurst)
	limitsdoc.Count(t, "leaseQPS", leaseQPS)
	limitsdoc.Count(t, "leaseBurst", leaseBurst)
	limitsdoc.Count(t, "klogMax", maxKlogVerbosity)

	limitsdoc.Duration(t, "probeTimeout", bindingProbeTimeout)
	limitsdoc.Duration(t, "probeGap", minRevisionGap)
	limitsdoc.Count(t, "threadWindow", threadFetchLimit)
	limitsdoc.Count(t, "statusMessage", maxStatusMessage)
	limitsdoc.Duration(t, "auditRetentionMin", MinAuditRetention)

	// The leader builds the retention loop without a reply age or a session grace, so the loop's defaults are the values.
	r, err := newRetention(retentionConfig{
		Store:      struct{ retentionStore }{},
		ThreadTTLs: func() (map[string]time.Duration, bool) { return nil, false },
		Canceller:  struct{ taskCanceller }{}, Audit: struct{ staleAuditor }{},
		Holds:     func(store.ConversationKey, string) bool { return false },
		OnRemoved: func(string, int) {},
	})
	if err != nil {
		t.Fatalf("newRetention: %v", err)
	}
	limitsdoc.Duration(t, "sessionGrace", r.cfg.SessionGrace)
	limitsdoc.Duration(t, "strandedReply", r.cfg.ReplyAge)
}
