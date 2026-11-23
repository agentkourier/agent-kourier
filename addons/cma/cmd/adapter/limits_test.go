package main

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

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
