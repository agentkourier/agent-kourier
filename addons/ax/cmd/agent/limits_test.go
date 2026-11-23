package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// TestLogLimits holds the cap on an error's text in a log line, which the kit's log handler applies to the agent's log,
// to the documentation.
func TestLogLimits(t *testing.T) {
	limitsdoc.Bytes(t, "ax.log-error-bytes", telemetry.MaxLogErrorBytes)
}

// TestChartLimits holds the add-on's startup probe in the chart to the pages, written as `30 tries, 2s apart`: how many
// times the kubelet tries it, and how far apart (in whole seconds), which bound a leader's takeover (the settlement of
// the turns a restart cut off).
func TestChartLimits(t *testing.T) {
	failures, period := limitsdoc.StartupProbe(t, "ax")
	limitsdoc.Text(t, "ax.startup-probe", fmt.Sprintf("%d tries, %ds apart", failures, int(period/time.Second)))
}

// TestEveryMarkerHasAGuard fails for a marker of this add-on (ax.) on a reference page that no test of this module holds
// to the code, and for a guard of a limit that the limits page does not have.
func TestEveryMarkerHasAGuard(t *testing.T) {
	limitsdoc.EveryMarkerHasAGuard(t, "ax.")
}
