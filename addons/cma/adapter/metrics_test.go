package adapter

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry/telemetrytest"
)

// The adapter's metric families are the ones it has always served: the same names, help, types, label names and
// label values. The registry is built as the adapter builds it, and every family it registers must count here.
func TestMetricFamiliesAreUnchanged(t *testing.T) {
	tel, err := telemetry.New(TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := &Executor{Telemetry: tel}
	if exec.Metrics, err = NewMetrics(tel, exec.Contexts); err != nil {
		t.Fatal(err)
	}
	for _, o := range []telemetry.TurnOutcome{telemetry.TurnCompleted, telemetry.TurnInputRequired, telemetry.TurnFailed,
		telemetry.TurnCanceled, turnTimeout} {
		_, turn := tel.StartTurn(t.Context(), "t", "c")
		turn.End(o)
	}
	for _, op := range []telemetry.Operation{opCreateSession, opStream, opSend, opListSessions, opArchive} {
		_, call := tel.StartCall(t.Context(), op)
		call.End("success")
	}
	// The adapter's check verifies tokens alone, so it never refuses for want of an issuer (auth.ReasonUnavailable).
	for _, r := range []auth.Reason{auth.ReasonMissing, auth.ReasonInvalid} {
		tel.AuthRefused(string(r))
	}
	for _, r := range []ArchiveReason{ArchiveIdle, ArchiveSuperseded, ArchiveGone} {
		exec.Metrics.SessionArchived(r)
	}
	tel.MessageReplay()
	tel.TasksInterrupted(1)
	tel.SetLeader(true)
	tel.Takeover()
	for _, o := range []telemetry.ForwardOutcome{telemetry.Forwarded, telemetry.ForwardFailed, telemetry.ForwardCancelled,
		telemetry.ForwardNoLeader, telemetry.ForwardLoop} {
		tel.Forward(o)
	}
	telemetrytest.Families(t, tel.Registry(), "agentkourier_cma_", "testdata/metrics.golden")
}
