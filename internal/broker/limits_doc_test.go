package broker

import (
	"fmt"
	"net/http"
	"testing"

	"k8s.io/client-go/tools/leaderelection"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the broker's wiring. Each value is the one the
// broker runs with: a default that production leaves unset is read where it is filled in (the election's timings), and a
// constant is the one production reads. The sync timeout is not here: its default is the binary's own (cmd/agent-kourier),
// which that package's test guards, and neither are the retention loop's limits, which internal/retention's test guards.
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
	limitsdoc.Duration(t, "statusGap", statusGap)
	limitsdoc.Duration(t, "statusTick", statusTick)
	limitsdoc.Duration(t, "statusBackoffCap", statusBackoffCap)
	limitsdoc.Count(t, "fightPutBacks", fightPutBacks)
	limitsdoc.Count(t, "fightWindow", fightWindow)
	limitsdoc.Duration(t, "superviseEvery", defaultSuperviseEvery)
	limitsdoc.Duration(t, "identifyTimeout", identifyTimeout)
	limitsdoc.Duration(t, "runtimeStopTimeout", runtimeStopTimeout)
	limitsdoc.Duration(t, "reconnectBase", defaultRunBase)
	limitsdoc.Duration(t, "reconnectMax", defaultRunMax)
	limitsdoc.Duration(t, "reconnectReset", defaultRunResetAfter)
	limitsdoc.Duration(t, "retryAfterMax", retryAfterMax)
	limitsdoc.Duration(t, "slackAPITimeout", slackAPITimeout)

	// The chat listener's: the server's bounds as the server Run serves is built with them, and those the route table holds itself.
	chat := newChatServer(http.NotFoundHandler())
	limitsdoc.Bytes(t, "chatHeaders", chat.MaxHeaderBytes)
	limitsdoc.Duration(t, "chatReadHeader", chat.ReadHeaderTimeout)
	limitsdoc.Duration(t, "chatRead", chat.ReadTimeout)
	limitsdoc.Duration(t, "chatWrite", chat.WriteTimeout)
	limitsdoc.Duration(t, "chatIdle", chat.IdleTimeout)
	limitsdoc.Bytes(t, "chatBody", chatMaxBody)
	limitsdoc.Duration(t, "chatHandler", chatHandlerTimeout)
	routes := newChatRoutes(nil, nil)
	limitsdoc.Count(t, "chatInFlight", cap(routes.slots))
	limitsdoc.Bytes(t, "chatBodyBudget", routes.budget.left)
	limitsdoc.Bytes(t, "chatBodyChunk", chatBodyChunk)
	limitsdoc.Duration(t, "chatDrain", chatDrainTimeout)
}
