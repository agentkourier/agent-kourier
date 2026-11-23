package agent

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// TelemetrySpec names the agent's telemetry: metrics agentkourier_ax_*, spans agentkourier.ax.* (a call to the model's
// provider is agentkourier.ax.model.<operation>), and the help of the metrics whose words are the agent's.
func TelemetrySpec() telemetry.Spec {
	return telemetry.Spec{
		Name:            "ax",
		Calls:           "model",
		TurnsHelp:       "Turns the agent ran, by how they ended: completed, input_required (paused on an ask_user question), failed, canceled or interrupted (failed by the leader after a restart or a takeover).",
		OperationHelp:   "Duration of the agent's calls to its model's provider, Amazon Bedrock (converse, converse_stream) or the Anthropic API (messages_stream), a stream timed while it opens, by outcome: success, cancelled, timeout, the HTTP status the provider answered, or error.",
		AuthRefusedHelp: "Requests to the A2A endpoint or the agent card that the caller check refused, by reason: missing (no credential, 401), invalid (not accepted, 401) or unavailable (the OIDC issuer could not be reached, 503).",
	}
}

// DeleteReason is why the agent deleted a context's ADK session: the label of agentkourier_ax_sessions_deleted_total.
type DeleteReason string

// The reasons, a closed set.
const (
	// DeleteIdle is a session deleted, with its context's tasks, after SESSION_IDLE_TTL without activity.
	DeleteIdle DeleteReason = "idle"
)

// Metrics are the agent's own metrics, beside the kit's: the ADK sessions it deleted. A nil *Metrics records nothing.
type Metrics struct {
	deleted *prometheus.CounterVec
}

// NewMetrics registers the agent's own metric in tel: agentkourier_ax_sessions_deleted_total.
func NewMetrics(tel *telemetry.Telemetry) (*Metrics, error) {
	m := &Metrics{deleted: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_sessions_deleted_total",
		Help: "ADK sessions the agent deleted, each with its context's tasks, by reason: idle (no activity for SESSION_IDLE_TTL)."},
		[]string{"reason"})}
	if err := tel.Register(m.deleted); err != nil {
		return nil, err
	}
	return m, nil
}

// SessionsDeleted counts n sessions deleted for reason.
func (m *Metrics) SessionsDeleted(reason DeleteReason, n int) {
	if m != nil && n > 0 {
		m.deleted.WithLabelValues(string(reason)).Add(float64(n))
	}
}
