package broker

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
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
	bindingCond   *prometheus.GaugeVec
	bindingReady  *prometheus.GaugeVec
	probeDuration *prometheus.HistogramVec
	invalid       *prometheus.GaugeVec
	bindingRej    *prometheus.GaugeVec
}

// The kinds and reasons that the config gauges carry, a closed set each.
var (
	configKinds      = []string{config.KindAgentBackend, config.KindChatConnection, config.KindBinding}
	rejectionReasons = []config.RejectionReason{config.ReasonInvalidSpec, config.ReasonBackendNotFound, config.ReasonBackendRejected,
		config.ReasonConnectionNotFound, config.ReasonConnectionRejected, config.ReasonNamespaceNotAllowed, config.ReasonChannelConflict}
)

// NewMetrics returns the metrics. stale is read at each scrape for agentkourier_config_stale, and
// may be nil (never stale): it is Config.Stale.
func NewMetrics(stale func() bool) (*Metrics, error) {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		auditFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_audit_write_failures_total",
			Help: "Audit entries that could not be written, by kind."}, []string{"kind"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_credential_rejections_total",
			Help: "Credentials an agent rejected, by Binding."}, []string{"binding"}),
		abandons: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_outbox_abandoned_total",
			Help: "Outbound posts given up on, by reason."}, []string{"reason"}),
		reloadErrors: prometheus.NewCounter(prometheus.CounterOpts{Name: "agentkourier_config_reload_errors_total",
			Help: "Config reloads that failed."}),
		approvals: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_approval_outcomes_total",
			Help: "What became of pauses, by Binding and outcome: turned away by the Binding's policy (auto_denied, ask_user_cancelled), or a tool approval that was decided (approved, denied, partially_approved) or timed out (timed_out)."}, []string{"binding", "outcome"}),
		timeouts: prometheus.NewCounter(prometheus.CounterOpts{Name: "agentkourier_interaction_timeouts_total",
			Help: "Interactions that timed out unanswered."}),
		removed: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_retention_removed_total",
			Help: "Stored content removed by retention, by what: stranded_replies, cleared_interactions, audit_entries, expired_sessions, session_replies, session_interactions, session_presses, session_outbox, stale_tasks (a task left on an expired session, ended) or stale_task_interactions (pending interactions cancelled with it)."}, []string{"what"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_interaction_decisions_total",
			Help: "Presses decided on an interaction, by result."}, []string{"result"}),
		chatUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentkourier_chat_connected",
			Help: "1 while the chat connection (Slack Socket Mode) is up, else 0, by ChatConnection."}, []string{"connection"}),
		turns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_turn_outcomes_total",
			Help: "Turns that ended, by Binding and how: the terminal state of the task, or the class of what stopped it."}, []string{"binding", "outcome"}),
		leader: prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentkourier_leader",
			Help: "1 while this replica does Agent Kourier's work (it leads, or it is the only replica: SQLite), 0 while it is a standby."}),
		takeovers: prometheus.NewCounter(prometheus.CounterOpts{Name: "agentkourier_leader_takeovers_total",
			Help: "Times this replica took the leadership: it won the lease and was given the next epoch."}),
		bindingCond: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentkourier_binding_condition",
			Help: "1 for the status a Binding's condition has (true, false or unknown) and 0 for the other two, by Binding and condition. Only the leader reports it. A condition has series once its first probe has ended; Ready has them once every first probe of the Binding has ended or one found it false."}, []string{"binding", "condition", "status"}),
		bindingReady: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentkourier_binding_ready",
			Help: "1 while every condition of the Binding is true, 0 while one is false or not yet known, by Binding. Only the leader reports it. A Binding has the series once every first probe of it has ended or one found it false."}, []string{"binding"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentkourier_binding_probe_duration_seconds",
			Help: "How long a probe of a Binding took, by condition: the time the checker took, whatever it found.", Buckets: prometheus.DefBuckets}, []string{"condition"}),
		invalid: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentkourier_config_invalid_objects",
			Help: "Objects of the custom resource config source that fail Default or Validate, by kind: each is served from its last good generation, or left out if it has none. 0 while the config is read from files, where one bad object fails the whole reload."}, []string{"kind"}),
		bindingRej: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentkourier_config_binding_rejections",
			Help: "Bindings of the custom resource config source that are not accepted as written, by the reason of their Accepted condition: InvalidSpec, BackendNotFound, BackendRejected, ConnectionNotFound, ConnectionRejected, NamespaceNotAllowed or ChannelConflict. A Binding that is served from an older generation counts too. 0 while the config is read from files."}, []string{"reason"}),
	}
	m.ConfigReport(config.Report{})
	staleGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "agentkourier_config_stale",
		Help: "1 while the latest config reload failed, so the running config lags the files."}, func() float64 {
		if stale != nil && stale() {
			return 1
		}
		return 0
	})
	var err error
	for _, c := range []prometheus.Collector{m.auditFailures, m.rejections, m.abandons, m.reloadErrors, m.approvals, m.timeouts, m.removed, m.decisions, m.chatUp, m.turns, m.leader, m.takeovers, m.bindingCond, m.bindingReady, m.probeDuration, m.invalid, m.bindingRej, staleGauge} {
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

// ConfigReport sets the gauges of what the config source refused: BootstrapOptions.OnReport. Every kind and every reason has
// its series, set at each report, so that one that cleared reads 0 and no series comes or goes. A reason outside the closed
// set is counted as "other".
func (m *Metrics) ConfigReport(r config.Report) {
	kinds := make(map[string]int, len(configKinds))
	for _, inv := range r.Invalid {
		kinds[inv.Kind]++
	}
	for _, k := range configKinds {
		m.invalid.WithLabelValues(k).Set(float64(kinds[k]))
	}
	reasons := make(map[string]int, len(rejectionReasons)+1)
	for _, rej := range r.Rejections {
		label := string(rej.Reason)
		if !slices.Contains(rejectionReasons, rej.Reason) {
			label = otherLabel
		}
		reasons[label]++
	}
	for _, reason := range rejectionReasons {
		m.bindingRej.WithLabelValues(string(reason)).Set(float64(reasons[string(reason)]))
	}
	m.bindingRej.WithLabelValues(otherLabel).Set(float64(reasons[otherLabel]))
}

// ApprovalOutcome counts what became of a pause, by Binding: one a Binding's policy turned away (interact.Config.OnApprovalOutcome),
// a tool approval that a person decided (interact.DecideConfig.OnApprovalOutcome), or one that timed out on a Binding that
// allows approvals (interact.SweepConfig.OnApprovalOutcome). An outcome outside the closed set
// (interact's Outcome constants) is one series, "other".
func (m *Metrics) ApprovalOutcome(binding string, outcome interact.Outcome) {
	label := string(outcome)
	switch outcome {
	case interact.OutcomeAskUserCancelled, interact.OutcomeAutoDenied,
		interact.OutcomeApproved, interact.OutcomeDenied, interact.OutcomePartiallyApproved, interact.OutcomeTimedOut:
	default:
		label = otherLabel
	}
	m.approvals.WithLabelValues(binding, label).Inc()
}

// InteractionDecision counts a press that was decided: interact.DecideConfig.OnDecision. The results are
// the closed set that DecideConfig documents; any other is one series, "other".
func (m *Metrics) InteractionDecision(result string) {
	switch result {
	case "answered", "superseded", "late", "invalid", "unrecorded":
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

// ChatConnected sets agentkourier_chat_connected for the ChatConnection named connection ("namespace/name"): 1 when the
// platform's connection is up, 0 when it is not. The adapter's hook for it is Config.OnConnection of the Slack adapter.
func (m *Metrics) ChatConnected(connection string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.chatUp.WithLabelValues(connection).Set(v)
}

// Leader sets agentkourier_leader: 1 once this replica does Agent Kourier's work, 0 while it waits to.
func (m *Metrics) Leader(leading bool) {
	v := 0.0
	if leading {
		v = 1
	}
	m.leader.Set(v)
}

// Takeover counts a leadership taken: the lease won and the epoch bumped.
func (m *Metrics) Takeover() { m.takeovers.Inc() }

// BindingCondition records a condition of a Binding that changed: bindingcheck.Options.OnTransition. The condition's series
// say its new status (1 for it, 0 for the other two), and a change of Ready also sets agentkourier_binding_ready, which is 1
// for True and 0 for False and Unknown. The reason is not a label: a reason that flaps would make series come and go, and
// it is in the log line of the transition.
//
// A Ready that is still Pending (no condition is False, and a first probe has not ended: bindingcheck names a Pending condition
// before any other Unknown, so the order in which the probes end does not matter) gets no series of its own and no ready gauge: the
// Binding is not known to be unready, only not yet known, and a rule on == 0 would fire after every restart for the length of the slower
// probe. A Binding that a first result has already found False has the series at once, with 0, which is true. The other conditions'
// series are exported as they end. Ready never becomes Pending again.
func (m *Metrics) BindingCondition(tr bindingcheck.Transition) {
	if tr.New.Type == bindingcheck.Ready && tr.New.Pending() {
		return
	}
	for _, s := range [...]bindingcheck.Status{bindingcheck.True, bindingcheck.False, bindingcheck.Unknown} {
		v := 0.0
		if tr.New.Status == s {
			v = 1
		}
		m.bindingCond.WithLabelValues(tr.Binding, string(tr.New.Type), strings.ToLower(string(s))).Set(v)
	}
	if tr.New.Type == bindingcheck.Ready {
		v := 0.0
		if tr.New.Status == bindingcheck.True {
			v = 1
		}
		m.bindingReady.WithLabelValues(tr.Binding).Set(v)
	}
}

// BindingsPruned deletes every series of the Bindings that left the config: bindingcheck.Options.OnPrune. The Prober calls it after
// the last transition it reports for a Binding, so a series it deletes is not set again.
func (m *Metrics) BindingsPruned(names []string) {
	for _, name := range names {
		m.bindingCond.DeletePartialMatch(prometheus.Labels{"binding": name})
		m.bindingReady.DeleteLabelValues(name)
	}
}

// BindingProbed observes how long a probe of a condition took.
func (m *Metrics) BindingProbed(cond bindingcheck.ConditionType, d time.Duration) {
	m.probeDuration.WithLabelValues(string(cond)).Observe(d.Seconds())
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
