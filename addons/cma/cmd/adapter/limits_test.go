package main

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// TestLimitsPage holds the server's timeouts to docs/reference/limits.md: the wait for a request's headers, for the
// requests in flight at a graceful stop, and for the hand-over of the lease after them.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "cma.read-header-timeout", readHeaderTimeout)
	limitsdoc.Duration(t, "cma.shutdown-grace", shutdownTimeout)
	limitsdoc.Duration(t, "cma.release-timeout", releaseTimeout)
}

// TestForwardLimits holds how long a standby waits to connect to the leader before it answers 503, the kit's router's
// constant, to docs/reference/limits.md.
func TestForwardLimits(t *testing.T) {
	limitsdoc.Duration(t, "cma.forward-dial-timeout", cluster.ForwardDialTimeout)
}

// TestLogLimits holds the cap on an error's text in a log line, which the kit's log handler applies to the adapter's
// log, to docs/reference/limits.md.
func TestLogLimits(t *testing.T) {
	limitsdoc.Bytes(t, "cma.log-error-bytes", telemetry.MaxLogErrorBytes)
}

// TestChartLimits holds the add-on's startup probe in the chart to the pages: how many times the kubelet tries it, and
// how far apart, which bound the start-up work.
func TestChartLimits(t *testing.T) {
	failures, period := limitsdoc.StartupProbe(t, "cma")
	limitsdoc.Count(t, "cma.startup-failures", failures)
	limitsdoc.Duration(t, "cma.startup-period", period)
}

// TestEveryMarkerHasAGuard fails for a marker of this add-on (cma.) on a reference page that no test of this module
// holds to the code, and for a guard of a limit that the limits page does not have.
func TestEveryMarkerHasAGuard(t *testing.T) {
	limitsdoc.EveryMarkerHasAGuard(t, "cma.")
}
