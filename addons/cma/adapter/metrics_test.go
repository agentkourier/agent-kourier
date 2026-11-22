package adapter

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry/telemetrytest"
)

// The adapter's metric families are the ones it has always served: the same names, help, types, label names and
// label values. The registry is built as the adapter builds it, and every family it registers must count here.
func TestMetricFamiliesAreUnchanged(t *testing.T) {
	tel, err := telemetry.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := &Executor{Telemetry: tel}
	if err := tel.ObserveSessions(exec.Contexts); err != nil {
		t.Fatal(err)
	}
	for _, o := range []telemetry.TurnOutcome{telemetry.TurnCompleted, telemetry.TurnInputRequired, telemetry.TurnFailed,
		telemetry.TurnCanceled, telemetry.TurnTimeout} {
		_, turn := tel.StartTurn(t.Context(), "t", "c")
		turn.End(o)
	}
	for _, op := range []telemetry.Operation{telemetry.OpCreateSession, telemetry.OpStream, telemetry.OpSend,
		telemetry.OpListSessions, telemetry.OpArchive} {
		_, call := tel.StartCall(t.Context(), op)
		call.End("success")
	}
	for _, r := range []string{"missing", "invalid"} {
		tel.AuthRefused(telemetry.AuthReason(r))
	}
	for _, r := range []telemetry.ArchiveReason{telemetry.ArchiveIdle, telemetry.ArchiveSuperseded, telemetry.ArchiveGone} {
		tel.SessionArchived(r)
	}
	tel.MessageReplay()
	tel.TaskInterrupted()
	tel.SetLeader(true)
	tel.Takeover()
	for _, o := range []telemetry.ForwardOutcome{telemetry.Forwarded, telemetry.ForwardFailed, telemetry.ForwardCancelled,
		telemetry.ForwardNoLeader, telemetry.ForwardLoop} {
		tel.Forward(o)
	}
	telemetrytest.Families(t, tel.Registry(), "agentkourier_cma_", "testdata/metrics.golden")
}
