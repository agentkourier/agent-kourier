package broker

import (
	"errors"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// otherLabel is the value of a label whose input is outside the fixed set it takes.
const otherLabel = "other"

// Metrics is the broker's Prometheus registry and the hooks that feed it. It is not the global
// registry: each broker has its own, so tests and a second broker in one process do not collide.
// The labels are Binding names, audit kinds and the abandon reasons of abandonLabel, never a user or a
// channel. Its methods are safe for concurrent use.
type Metrics struct {
	reg           *prometheus.Registry
	auditFailures *prometheus.CounterVec
	rejections    *prometheus.CounterVec
	abandons      *prometheus.CounterVec
	reloadErrors  prometheus.Counter
	approvals     *prometheus.CounterVec
	timeouts      prometheus.Counter
	decisions     *prometheus.CounterVec
}

// NewMetrics returns the metrics. stale is read at each scrape for courier_config_stale, and
// may be nil (never stale): it is Config.Stale.
func NewMetrics(stale func() bool) (*Metrics, error) {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		auditFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_audit_write_failures_total",
			Help: "Audit entries that could not be written, by kind."}, []string{"kind"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_credential_rejections_total",
			Help: "Credentials an agent rejected, by Binding."}, []string{"binding"}),
		abandons: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_outbox_abandoned_total",
			Help: "Outbound posts given up on, by reason."}, []string{"reason"}),
		reloadErrors: prometheus.NewCounter(prometheus.CounterOpts{Name: "courier_config_reload_errors_total",
			Help: "Config reloads that failed."}),
		approvals: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_approval_outcomes_total",
			Help: "Pauses a Binding's policy turned away, by Binding and outcome."}, []string{"binding", "outcome"}),
		timeouts: prometheus.NewCounter(prometheus.CounterOpts{Name: "courier_interaction_timeouts_total",
			Help: "Interactions that timed out unanswered."}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_interaction_decisions_total",
			Help: "Presses decided on an interaction, by result."}, []string{"result"}),
	}
	staleGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "courier_config_stale",
		Help: "1 while the latest config reload failed, so the running config lags the files."}, func() float64 {
		if stale != nil && stale() {
			return 1
		}
		return 0
	})
	var err error
	for _, c := range []prometheus.Collector{m.auditFailures, m.rejections, m.abandons, m.reloadErrors, m.approvals, m.timeouts, m.decisions, staleGauge} {
		err = errors.Join(err, m.reg.Register(c))
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// AuditFailure counts a failed audit write: audit.Options.OnFailure.
func (m *Metrics) AuditFailure(kind store.AuditKind) {
	label := string(kind)
	if !kind.Valid() {
		label = otherLabel
	}
	m.auditFailures.WithLabelValues(label).Inc()
}

// CredentialRejected counts a credential an agent rejected: Options.OnRejected.
func (m *Metrics) CredentialRejected(binding string) { m.rejections.WithLabelValues(binding).Inc() }

// ReloadError counts a failed config reload: BootstrapOptions.OnReloadError.
func (m *Metrics) ReloadError(error) { m.reloadErrors.Inc() }

// ApprovalOutcome counts a pause a Binding's policy turned away: interact.Config.OnApprovalOutcome.
// An outcome outside the closed set is one series, "other".
func (m *Metrics) ApprovalOutcome(binding string, outcome interact.Outcome) {
	label := string(outcome)
	switch outcome {
	case interact.OutcomeAskUserCancelled, interact.OutcomeAutoDenied:
	default:
		label = otherLabel
	}
	m.approvals.WithLabelValues(binding, label).Inc()
}

// InteractionDecision counts a press that was decided: interact.DecideConfig.OnDecision. The results are
// the closed set that DecideConfig documents; any other is one series, "other".
func (m *Metrics) InteractionDecision(result string) {
	switch result {
	case "answered", "superseded", "late", "invalid":
	default:
		result = otherLabel
	}
	m.decisions.WithLabelValues(result).Inc()
}

// InteractionTimeout counts an interaction that timed out: interact's SweepConfig.OnTimeout. The ID is
// not a label.
func (m *Metrics) InteractionTimeout(string) { m.timeouts.Inc() }

// OutboxAbandoned counts an abandoned post: outbox.Config.OnAbandon. The row's ID is not a label.
func (m *Metrics) OutboxAbandoned(_, reason string) {
	m.abandons.WithLabelValues(abandonLabel(reason)).Inc()
}

// abandonLabel is reason if it is one the outbox gives: its own three, or a neutral reason of a permanent error
// (chat.Reason, whatever the platform's own code, which is in the log and not here), and "other" for anything else,
// so that no reason text can grow the series.
func abandonLabel(reason string) string {
	switch reason {
	case outbox.AbandonInvalid, outbox.AbandonLookupTimeout, outbox.AbandonExpired,
		string(chat.ReasonChannelGone), string(chat.ReasonTooLong), string(chat.ReasonInvalidContent), string(chat.ReasonOther):
		return reason
	}
	return otherLabel
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
