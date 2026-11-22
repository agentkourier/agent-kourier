package adapter

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// turnTimeout is a turn that waited ToolResultTimeout for a sandbox tool's result: an outcome of
// agentkourier_cma_turns_total that only this adapter has, which fails the turn's span.
const turnTimeout telemetry.TurnOutcome = "timeout"

// TelemetrySpec names the adapter's telemetry: metrics agentkourier_cma_*, spans agentkourier.cma.* (a call to the
// Managed Agents API is agentkourier.cma.backend.<operation>), and the help of the metrics whose words are the
// adapter's.
func TelemetrySpec() telemetry.Spec {
	return telemetry.Spec{
		Name:            "cma",
		Calls:           "backend",
		TurnsHelp:       "Turns of a CMA session the adapter ran, by how they ended: completed, input_required (paused on a tool approval), failed, canceled, timeout (no sandbox tool result for TOOL_RESULT_TIMEOUT) or interrupted (failed by the leader after a restart or a takeover).",
		OperationHelp:   "Duration of the adapter's calls to the Managed Agents API (create_session, stream (opening it), send, list_sessions, archive), by outcome: success, cancelled, timeout, the HTTP status the API answered, or error.",
		AuthRefusedHelp: "A2A requests the caller check refused (401), by reason: missing (no token in the header) or invalid (not an accepted token, or an Authorization header that is not Bearer).",
		ErrorTurns:      []telemetry.TurnOutcome{turnTimeout},
	}
}

// ArchiveReason is why a session was archived or let go: the label of agentkourier_cma_sessions_archived_total.
type ArchiveReason string

// The reasons, a closed set.
const (
	// ArchiveIdle is a session archived after SESSION_IDLE_TTL without a turn.
	ArchiveIdle ArchiveReason = "idle"
	// ArchiveSuperseded is a context's extra session, archived when the leader reconciled the store with CMA.
	ArchiveSuperseded ArchiveReason = "superseded"
	// ArchiveGone is a stored session that CMA no longer lists, forgotten when the leader reconciled.
	ArchiveGone ArchiveReason = "gone"
)

// Metrics are the adapter's own metrics, beside the kit's: the sessions it let go of, and the contexts that have a
// session. A nil *Metrics records nothing.
type Metrics struct {
	archived *prometheus.CounterVec
}

// NewMetrics registers the adapter's own metrics in tel: agentkourier_cma_sessions_archived_total, and
// agentkourier_cma_sessions, a gauge that reads sessions when it is scraped (Executor.Contexts: a standby's is
// none).
func NewMetrics(tel *telemetry.Telemetry, sessions func() int) (*Metrics, error) {
	m := &Metrics{archived: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_cma_sessions_archived_total",
		Help: "Sessions the adapter let go of, by reason: idle (archived after SESSION_IDLE_TTL), superseded (a context's extra session, archived at reconciliation) or gone (a stored session CMA no longer lists, forgotten at reconciliation)."},
		[]string{"reason"})}
	gauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "agentkourier_cma_sessions",
		Help: "A2A contexts that have a CMA session in this replica's map: the leader's are every context the store holds; a standby's are none."},
		func() float64 { return float64(sessions()) })
	for _, c := range []prometheus.Collector{m.archived, gauge} {
		if err := tel.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// SessionArchived counts a session let go of.
func (m *Metrics) SessionArchived(reason ArchiveReason) {
	if m != nil {
		m.archived.WithLabelValues(string(reason)).Inc()
	}
}
