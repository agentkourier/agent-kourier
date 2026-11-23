package agent

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/ax/anthropic"
	"github.com/agentkourier/agent-kourier/addons/ax/bedrock"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry/telemetrytest"
)

// The agent's metric families are the ones it has always served: the same names, help, types, label names and label
// values. The registry is built as the agent builds it, and every family it registers must count here.
func TestMetricFamiliesAreUnchanged(t *testing.T) {
	tel, err := telemetry.New(TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(tel)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []telemetry.TurnOutcome{telemetry.TurnCompleted, telemetry.TurnInputRequired, telemetry.TurnFailed,
		telemetry.TurnCanceled} {
		_, turn := tel.StartTurn(t.Context(), "t", "c")
		turn.End(o)
	}
	for _, op := range []telemetry.Operation{bedrock.OpConverse, bedrock.OpConverseStream, anthropic.OpMessagesStream} {
		_, call := tel.StartCall(t.Context(), op)
		call.End("success")
	}
	for _, r := range []auth.Reason{auth.ReasonMissing, auth.ReasonInvalid, auth.ReasonUnavailable} {
		tel.AuthRefused(string(r))
	}
	metrics.SessionsDeleted(DeleteIdle, 1)
	tel.MessageReplay()
	tel.TasksInterrupted(1)
	tel.SetLeader(true)
	tel.Takeover()
	for _, o := range []telemetry.ForwardOutcome{telemetry.Forwarded, telemetry.ForwardFailed, telemetry.ForwardCancelled,
		telemetry.ForwardNoLeader, telemetry.ForwardLoop} {
		tel.Forward(o)
	}
	telemetrytest.Families(t, tel.Registry(), "agentkourier_ax_", "testdata/metrics.golden")
}
