package broker

import (
	"errors"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// otherLabel is the value of a label whose input is outside the fixed set it takes.
const otherLabel = "other"

// Metrics is the broker's Prometheus registry and the hooks that feed it. It is not the global
// registry: each broker has its own, so tests and a second broker in one process do not collide.
// The labels are Binding names, the ChatConnection's name, audit kinds and the abandon reasons of abandonLabel, never a
// user or a channel. Its methods are safe for concurrent use.
type Metrics struct {
	reg           *prometheus.Registry
	auditFailures *prometheus.CounterVec
	rejections    *prometheus.CounterVec
	abandons      *prometheus.CounterVec
	reloadErrors  prometheus.Counter
	approvals     *prometheus.CounterVec
	timeouts      prometheus.Counter
	removed       *prometheus.CounterVec
	decisions     *prometheus.CounterVec
	chatUp        *prometheus.GaugeVec
	turns         *prometheus.CounterVec
	leader        prometheus.Gauge
	takeovers     prometheus.Counter
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
		removed: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_retention_removed_total",
			Help: "Stored content removed by retention, by what: stranded_replies, cleared_interactions, audit_entries, expired_sessions, session_replies, session_interactions, session_presses or session_outbox."}, []string{"what"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_interaction_decisions_total",
			Help: "Presses decided on an interaction, by result."}, []string{"result"}),
		chatUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "courier_chat_connected",
			Help: "1 while the chat connection (Slack Socket Mode) is up, else 0, by ChatConnection."}, []string{"connection"}),
		turns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "courier_turn_outcomes_total",
			Help: "Turns that ended, by Binding and how: the terminal state of the task, or the class of what stopped it."}, []string{"binding", "outcome"}),
		leader: prometheus.NewGauge(prometheus.GaugeOpts{Name: "courier_leader",
			Help: "1 while this replica does courier's work (it leads, or it is the only replica: SQLite), 0 while it is a standby."}),
		takeovers: prometheus.NewCounter(prometheus.CounterOpts{Name: "courier_leader_takeovers_total",
			Help: "Times this replica took the leadership: it won the lease and was given the next epoch."}),
	}
	staleGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "courier_config_stale",
		Help: "1 while the latest config reload failed, so the running config lags the files."}, func() float64 {
		if stale != nil && stale() {
			return 1
		}
		return 0
	})
	var err error
	for _, c := range []prometheus.Collector{m.auditFailures, m.rejections, m.abandons, m.reloadErrors, m.approvals, m.timeouts, m.removed, m.decisions, m.chatUp, m.turns, m.leader, m.takeovers, staleGauge} {
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

// TurnOutcome counts a turn that ended: session.Config.OnTurnEnd. An outcome outside session.TurnOutcomes is one
// series, "other".
func (m *Metrics) TurnOutcome(binding string, outcome session.TurnOutcome) {
	label := string(outcome)
	if !outcome.Valid() {
		label = otherLabel
	}
	m.turns.WithLabelValues(binding, label).Inc()
}

// InteractionTimeout counts an interaction that timed out: interact's SweepConfig.OnTimeout. The ID is
// not a label.
func (m *Metrics) InteractionTimeout(string) { m.timeouts.Inc() }

// RetentionRemoved counts n items that retention removed: what is one of the Removed constants, a closed set.
func (m *Metrics) RetentionRemoved(what string, n int) {
	m.removed.WithLabelValues(what).Add(float64(n))
}

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

// ChatConnected sets courier_chat_connected for the ChatConnection named connection ("namespace/name"): 1 when the
// platform's connection is up, 0 when it is not. The adapter's hook for it is Config.OnConnection of the Slack adapter.
func (m *Metrics) ChatConnected(connection string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.chatUp.WithLabelValues(connection).Set(v)
}

// Leader sets courier_leader: 1 once this replica does courier's work, 0 while it waits to.
func (m *Metrics) Leader(leading bool) {
	v := 0.0
	if leading {
		v = 1
	}
	m.leader.Set(v)
}

// Takeover counts a leadership taken: the lease won and the epoch bumped.
func (m *Metrics) Takeover() { m.takeovers.Inc() }

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
